module github.com/picatz/websocket/benchmarks

go 1.27.0

replace github.com/picatz/websocket => ../

require (
	github.com/coder/websocket v1.8.15
	github.com/gobwas/ws v1.4.0
	github.com/gorilla/websocket v1.5.3
	github.com/picatz/websocket v0.0.0-20261016031137-2cdcc297f941
)

require (
	github.com/gobwas/httphead v0.1.0 // indirect
	github.com/gobwas/pool v0.2.1 // indirect
	golang.org/x/sys v0.6.0 // indirect
)
