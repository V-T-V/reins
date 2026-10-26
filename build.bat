@echo off
set GOOS=linux
set GOARCH=amd64
set CGO_ENABLED=0
cd /d D:\M_X_M\reins
go build -o bin\reins-relay-linux .\relay
echo BUILD_DONE
