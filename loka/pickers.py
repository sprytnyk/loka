import json
import os
import shutil
import subprocess
import sys
import webbrowser


def _desktop_environment():
    if not (getattr(sys, "frozen", False) and sys.platform.startswith("linux")):
        return None
    environment = os.environ.copy()
    original = environment.get("LD_LIBRARY_PATH_ORIG")
    if original is None:
        environment.pop("LD_LIBRARY_PATH", None)
    else:
        environment["LD_LIBRARY_PATH"] = original
    return environment


def open_browser(url):
    environment = _desktop_environment()
    if environment is None:
        webbrowser.open(url)
        return

    bundled_path = os.environ.pop("LD_LIBRARY_PATH", None)
    try:
        if "LD_LIBRARY_PATH" in environment:
            os.environ["LD_LIBRARY_PATH"] = environment["LD_LIBRARY_PATH"]
        webbrowser.open(url)
    finally:
        os.environ.pop("LD_LIBRARY_PATH", None)
        if bundled_path is not None:
            os.environ["LD_LIBRARY_PATH"] = bundled_path


def _linux_paths(files, title):
    separator = "\x1f"
    zenity = ["--multiple", f"--separator={separator}"] if files else ["--directory"]
    kdialog = (
        ["--getopenfilename", "", "", "--multiple", "--separate-output"]
        if files
        else ["--getexistingdirectory"]
    )
    for command, delimiter in (
        (["zenity", "--file-selection", f"--title={title}", *zenity], separator),
        (["kdialog", "--title", title, *kdialog], "\n"),
    ):
        if not shutil.which(command[0]):
            continue
        try:
            result = subprocess.run(
                command, capture_output=True, check=False, env=_desktop_environment()
            )
        except OSError:
            continue
        if result.returncode == 0:
            selected = os.fsdecode(result.stdout).removesuffix("\n")
            if not selected:
                return []
            return selected.split(delimiter) if files else [selected]
        if result.returncode == 1 and not result.stderr:
            return []
    raise OSError("No desktop picker available")


def _mac_paths(files, title):
    prompt = json.dumps(title)
    choice = (
        f"app.chooseFile({{withPrompt: {prompt}, multipleSelectionsAllowed: true}})"
        if files
        else f"[app.chooseFolder({{withPrompt: {prompt}}})]"
    )
    script = f"""
const app = Application.currentApplication();
app.includeStandardAdditions = true;
app.activate();
const selected = {choice};
JSON.stringify(selected.map(path => path.toString()));
"""
    result = subprocess.run(
        ["osascript", "-l", "JavaScript", "-e", script],
        capture_output=True,
        check=False,
    )
    if result.returncode and result.stderr.rstrip().endswith(b"(-128)"):
        return []
    result.check_returncode()
    return json.loads(result.stdout)


def _windows_paths(files, title):
    if files:
        dialog = f"""
$dialog = New-Object System.Windows.Forms.OpenFileDialog
$dialog.Title = '{title}'
$dialog.Multiselect = $true
"""
        result_field = "FileNames"
    else:
        dialog = f"""
$dialog = New-Object System.Windows.Forms.FolderBrowserDialog
$dialog.Description = '{title}'
"""
        result_field = "SelectedPath"
    script = f"""
$ErrorActionPreference = 'Stop'
[Console]::OutputEncoding = [Text.UTF8Encoding]::new($false)
Add-Type -AssemblyName System.Windows.Forms
{dialog}
$paths = @()
try {{
    if ($dialog.ShowDialog() -eq 'OK') {{ $paths = @($dialog.{result_field}) }}
}} finally {{
    $dialog.Dispose()
}}
[Console]::WriteLine((ConvertTo-Json -InputObject $paths -Compress))
"""
    result = subprocess.run(
        ["powershell.exe", "-NoProfile", "-NonInteractive", "-STA", "-Command", script],
        capture_output=True,
        check=True,
    )
    return json.loads(result.stdout)


def choose_paths(kind):
    title = {
        "files": "Choose files",
        "directory": "Choose directory",
        "destination": "Choose destination",
    }[kind]
    files = kind == "files"
    if sys.platform.startswith("linux"):
        paths = _linux_paths(files, title)
    elif sys.platform == "darwin":
        paths = _mac_paths(files, title)
    elif sys.platform == "win32":
        paths = _windows_paths(files, title)
    else:
        raise OSError("No desktop picker available")
    if not isinstance(paths, list) or any(
        not isinstance(path, str) or not os.path.isabs(path) for path in paths
    ):
        raise ValueError("Unexpected desktop picker output")
    return paths
