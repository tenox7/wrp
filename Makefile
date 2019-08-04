all: wrp

wrp: wrp.go
	go build wrp.go

cross: 
	GOOS=linux GOARCH=amd64 go build -a -o wrp-amd64-linux wrp.go
	GOOS=freebsd GOARCH=amd64 go build -a -o wrp-amd64-freebsd wrp.go
	GOOS=openbsd GOARCH=amd64 go build -a -o wrp-amd64-openbsd wrp.go
	GOOS=darwin GOARCH=amd64 go build -a -o wrp-amd64-macos wrp.go
	GOOS=windows GOARCH=amd64 go build -a -o wrp-amd64-windows.exe wrp.go
	GOOS=linux GOARCH=arm go build -a -o wrp-arm-linux wrp.go

docker: wrp
	docker build -t tenox7/wrp:latest .

clean:
	rm -rf wrp-* wrp