@echo off
set GOOS=linux
set GOARCH=amd64
set CGO_ENABLED=0
cd /d D:\M_X_M\go-rmm
go build -o bin\go-rmm-relay-linux .\relay
echo BUILD_DONE
