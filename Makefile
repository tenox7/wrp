all: wrp

wrp: wrp.go statik
	go build wrp.go

cross: statik
	GOOS=linux GOARCH=amd64 go build -a -o wrp-amd64-linux wrp.go
	GOOS=freebsd GOARCH=amd64 go build -a -o wrp-amd64-freebsd wrp.go
	GOOS=openbsd GOARCH=amd64 go build -a -o wrp-amd64-openbsd wrp.go
	GOOS=darwin GOARCH=amd64 go build -a -o wrp-amd64-macos wrp.go
	GOOS=windows GOARCH=amd64 go build -a -o wrp-amd64-windows.exe wrp.go
	GOOS=linux GOARCH=arm go build -a -o wrp-arm-linux wrp.go
	GOOS=linux GOARCH=arm64 go build -a -o wrp-arm64-linux wrp.go

statik: wrp.html
	go generate

docker: wrp
	docker build -t tenox7/wrp:latest .

dockerhub:
	docker push tenox7/wrp:latest

gcrio:
	docker tag tenox7/wrp:latest gcr.io/tenox7/wrp
	docker push gcr.io/tenox7/wrp

clean:
	rm -rf wrp-* wrp statik
