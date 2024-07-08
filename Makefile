all: wrp

wrp: wrp.go
	go build -a

cross:
	GOOS=linux GOARCH=amd64 go build -a -o wrp-amd64-linux
	GOOS=freebsd GOARCH=amd64 go build -a -o wrp-amd64-freebsd
	GOOS=openbsd GOARCH=amd64 go build -a -o wrp-amd64-openbsd
	GOOS=darwin GOARCH=amd64 go build -a -o wrp-amd64-macos
	GOOS=darwin GOARCH=arm64 go build -a -o wrp-arm64-macos
	GOOS=windows GOARCH=amd64 go build -a -o wrp-amd64-windows.exe
	GOOS=linux GOARCH=arm go build -a -o wrp-arm-linux
	GOOS=linux GOARCH=arm64 go build -a -o wrp-arm64-linux

docker-local:
	docker buildx build --platform linux/amd64,linux/arm64 -t tenox7/wrp:latest --load .

docker-push:
	docker buildx build --platform linux/amd64,linux/arm64 -t tenox7/wrp:latest --push .

clean:
	rm -rf wrp-* wrp
