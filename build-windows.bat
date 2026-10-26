@echo off
cd /d D:\M_X_M\reins
set GOOS=windows
set GOARCH=amd64
set CGO_ENABLED=0
go build -ldflags "-s -w -X main.version=deploy" -o bin\reins-agent-windows-amd64.exe .\agent
echo AGENT DONE
go build -ldflags "-s -w -X main.version=deploy" -o bin\reins-windows-amd64.exe .\cmd\reins
echo CLI DONE
go build -ldflags "-s -w -X main.version=deploy" -o bin\reins-relay-windows-amd64.exe .\relay
echo RELAY DONE
echo ALL DONE
