param (
    [switch]$clean = $false
)
$env:GOARCH="amd64"
foreach($os in ("linux", "freebsd", "openbsd", "darwin", "windows")) {
    $env:GOOS=$os
    $o="wrp-$(if ($os -eq "windows") {$os="windows.exe"} elseif ($os -eq "darwin") { $os="macos" })$os"
    Remove-Item -ErrorAction Ignore $o
    if (!$clean) {
        Invoke-Expression "& go build -a -o $o wrp.go"
    }
}

$env:GOARCH="arm"
$env:GOOS="linux"
$o="wrp-linux-rpi"
Remove-Item -ErrorAction Ignore  $o
if (!$clean) {
    Invoke-Expression "& go build -a -o $o wrp.go"
}
