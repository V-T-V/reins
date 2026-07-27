@echo off
cd /d D:\M_X_M\go-rmm
set GOOS=windows
set GOARCH=amd64
set CGO_ENABLED=0
go build -ldflags "-s -w -X main.version=deploy" -o bin\go-rmm-agent-windows-amd64.exe .\agent
echo AGENT DONE
go build -ldflags "-s -w -X main.version=deploy" -o bin\go-rmm-windows-amd64.exe .\cmd\go-rmm
echo CLI DONE
go build -ldflags "-s -w -X main.version=deploy" -o bin\go-rmm-relay-windows-amd64.exe .\relay
echo RELAY DONE
echo ALL DONE
