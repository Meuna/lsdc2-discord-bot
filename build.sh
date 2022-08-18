# /bin/bash
go build -ldflags "-s -w" -o build/frontend frontend/frontend.go
go build -ldflags "-s -w" -o build/backend backend/backend.go
