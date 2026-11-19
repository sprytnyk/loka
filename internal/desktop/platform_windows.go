package desktop

import "os/exec"

// Pick shows the Windows picker through PowerShell. A hidden
// topmost window owns the dialog, so it opens in front of the browser.
// The answer is a JSON list in UTF-8 without a byte order mark.
func Pick(kind Kind) ([]string, error) {
	dialog := `$d = New-Object System.Windows.Forms.OpenFileDialog -Property @{Multiselect = $true; Title = '` + kind.title() + `'}
$field = 'FileNames'`
	if kind != Files {
		dialog = `$d = New-Object System.Windows.Forms.FolderBrowserDialog -Property @{Description = '` + kind.title() + `'}
$field = 'SelectedPath'`
	}
	script := `$ErrorActionPreference = 'Stop'
Add-Type -AssemblyName System.Windows.Forms
[Console]::OutputEncoding = New-Object System.Text.UTF8Encoding $false
$owner = New-Object System.Windows.Forms.Form -Property @{TopMost = $true}
` + dialog + `
$paths = @()
try {
    if ($d.ShowDialog($owner) -eq 'OK') { $paths = @($d.$field) }
} finally {
    $d.Dispose()
    $owner.Dispose()
}
[Console]::WriteLine((ConvertTo-Json -InputObject $paths -Compress))`
	out, err := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-STA", "-Command", script).Output()
	if err != nil {
		return nil, err
	}
	return decodePaths(out)
}

// OpenBrowser opens url in the default web browser.
func OpenBrowser(url string) error {
	return start("rundll32", "url.dll,FileProtocolHandler", url)
}
