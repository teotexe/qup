module aurashare

go 1.26.4

require (
	github.com/godbus/dbus/v5 v5.2.2
	github.com/quic-go/quic-go v0.59.1
	github.com/teotexe/wayland-portal-go v0.0.0
)

replace github.com/teotexe/wayland-portal-go => ../wayland-portal-go

require (
	golang.org/x/crypto v0.41.0 // indirect
	golang.org/x/net v0.43.0 // indirect
	golang.org/x/sys v0.35.0 // indirect
)
