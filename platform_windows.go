package main

import "os/exec"

// pickWithSystem shows the Windows picker through PowerShell. A hidden
// topmost window owns the dialog, so it opens in front of the browser.
func pickWithSystem(folder bool) ([]string, error) {
	dialog := `$d = New-Object System.Windows.Forms.OpenFileDialog -Property @{Multiselect = $true; Title = 'Choose files'}
if ($d.ShowDialog($owner) -eq 'OK') { $d.FileNames -join "` + "`n" + `" }`
	if folder {
		dialog = `$d = New-Object System.Windows.Forms.FolderBrowserDialog -Property @{Description = 'Choose a directory'}
if ($d.ShowDialog($owner) -eq 'OK') { $d.SelectedPath }`
	}
	script := `Add-Type -AssemblyName System.Windows.Forms
[Console]::OutputEncoding = [Text.Encoding]::UTF8
$owner = New-Object System.Windows.Forms.Form -Property @{TopMost = $true}
` + dialog
	out, err := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-STA", "-Command", script).Output()
	if err != nil {
		return nil, err
	}
	return lines(string(out)), nil
}

// openBrowser opens url in the default web browser.
func openBrowser(url string) error {
	return start("rundll32", "url.dll,FileProtocolHandler", url)
}
