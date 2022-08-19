# /bin/bash
go build -ldflags "-s -w" -o build/frontend frontend/frontend.go
go build -ldflags "-s -w" -o build/backend backend/backend.go

zip --junk-paths build/frontend.zip build/frontend
zip --junk-paths build/backend.zip build/backend
