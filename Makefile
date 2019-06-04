all: linux freebsd openbsd macos windows rpi

linux: 
	GOOS=linux GOARCH=amd64 go build -a -o wrp-linux wrp.go

freebsd: 
	GOOS=freebsd GOARCH=amd64 go build -a -o wrp-freebsd wrp.go

openbsd: 
	GOOS=openbsd GOARCH=amd64 go build -a -o wrp-openbsd wrp.go

macos: 
	GOOS=darwin GOARCH=amd64 go build -a -o wrp-macos wrp.go

windows: 
	GOOS=windows GOARCH=amd64 go build -a -o wrp-windows.exe wrp.go

rpi:
	GOOS=linux GOARCH=arm go build -a -o wrp-linux-rpi wrp.go

clean:
	rm -rf wrp-linux wrp-freebsd wrp-openbsd wrp-macos wrp-windows.exe wrp-linux-rpi
