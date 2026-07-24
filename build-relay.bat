@echo off
cd /d D:\M_X_M\go-rmm
set GOOS=linux
set GOARCH=amd64
set CGO_ENABLED=0
go build -o bin\go-rmm-relay-linux .\relay
echo BUILD DONE
