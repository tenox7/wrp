$env:GOARCH="amd64"
foreach($os in ("linux", "freebsd", "openbsd", "darwin", "windows")) {
    $env:GOOS=$os
    Invoke-Expression "& go build -a -o wrp-$(if ($os -eq "windows") {$os="windows.exe"})$os wrp.go"
}

$env:GOARCH="arm"
$env:GOOS="linux"
Invoke-Expression "& go build -a -o wrp-linux-rpi wrp.go"