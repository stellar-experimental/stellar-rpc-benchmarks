module github.com/stellar/stellar-rpc-benchmarks/runner

go 1.26

// The campaign config parser — the only third-party dependency. Nothing
// imports it yet (internal/config does, from task 2 on), hence "indirect".
require github.com/BurntSushi/toml v1.6.0 // indirect
