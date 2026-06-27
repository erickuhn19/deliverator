module github.com/erickuhn19/deliverator

go 1.25.7

// Build with a patched toolchain: go1.25.7 has 8 reachable stdlib CVEs (TLS/x509/
// HTTP/net in the exchange-comms path); 1.25.11 fixes all of them. Pinned so CI and
// release builds are reproducible. Re-confirm zero reachable with `govulncheck`.
toolchain go1.25.11

require (
	github.com/BurntSushi/toml v1.6.0
	github.com/ethereum/go-ethereum v1.17.3
	github.com/gorilla/websocket v1.5.3
	github.com/shopspring/decimal v1.4.0
	github.com/spf13/cobra v1.10.2
	github.com/spf13/pflag v1.0.9
	github.com/vmihailenco/msgpack/v5 v5.4.1
	github.com/zalando/go-keyring v0.2.8
	golang.org/x/term v0.44.0
)

require (
	github.com/ProjectZKM/Ziren/crates/go-runtime/zkvm_runtime v0.0.0-20251001021608-1fe7b43fc4d6 // indirect
	github.com/bits-and-blooms/bitset v1.24.0 // indirect
	github.com/consensys/gnark-crypto v0.19.2 // indirect
	github.com/crate-crypto/go-eth-kzg v1.5.0 // indirect
	github.com/danieljoos/wincred v1.2.3 // indirect
	github.com/davecgh/go-spew v1.1.2-0.20180830191138-d8f796af33cc // indirect
	github.com/decred/dcrd/dcrec/secp256k1/v4 v4.4.0 // indirect
	github.com/ethereum/c-kzg-4844/v2 v2.1.6 // indirect
	github.com/godbus/dbus/v5 v5.2.2 // indirect
	github.com/holiman/uint256 v1.3.2 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/pmezard/go-difflib v1.0.1-0.20181226105442-5d4384ee4fb2 // indirect
	github.com/stretchr/objx v0.5.3 // indirect
	github.com/supranational/blst v0.3.16 // indirect
	github.com/vmihailenco/tagparser/v2 v2.0.0 // indirect
	golang.org/x/sync v0.19.0 // indirect
	golang.org/x/sys v0.46.0 // indirect
)
