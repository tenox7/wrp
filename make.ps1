param (
    [switch]$clean = $false
)
$env:GOARCH="amd64"
foreach($sys in ("amd64-linux", "arm-linux", "amd64-freebsd", "amd64-openbsd", "amd64-darwin", "amd64-windows")) {
    $cpu,$os = $sys.split('-')
    $env:GOARCH=$cpu
    $env:GOOS=$os    
    $o="wrp-$cpu-$(if ($os -eq "windows") {$os="windows.exe"} elseif ($os -eq "darwin") { $os="macos" })$os"
    Remove-Item -ErrorAction Ignore $o
    if (!$clean) {
        $cmd = "& go build -a -o $o wrp.go"
        Write-Host $cmd
        Invoke-Expression $cmd
    }
}

