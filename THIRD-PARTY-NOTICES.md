# Third-Party Notices

Deliverator (MIT-licensed) links the following third-party open-source libraries.
This file reproduces their attributions and licenses. Generated with
`go-licenses report ./...` (Go module graph of the shipped binary); regenerate when
dependencies change.

License texts are available at the linked sources and in each module's directory in
the Go module cache.

---

## ⚠️ github.com/ethereum/go-ethereum — LGPL-3.0 (library packages)

Deliverator uses the Ethereum Go library for EIP-712 / secp256k1 signing and related
primitives. go-ethereum is **dual-licensed**: its command-line programs (`cmd/...`)
are GPL-3.0, but the **library packages are LGPL-3.0** (`COPYING.LESSER`).

**Deliverator links only library packages** — verified with
`go list -deps ./... | grep go-ethereum/cmd` → empty. No GPL-licensed go-ethereum
code is included in the binary, so MIT-licensing Deliverator is compatible.

- License: **LGPL-3.0** — https://github.com/ethereum/go-ethereum/blob/v1.17.3/COPYING.LESSER
- Source (corresponding version): https://github.com/ethereum/go-ethereum/tree/v1.17.3
- Copyright © The go-ethereum Authors.

Per LGPL-3.0, the corresponding source of this version is available at the link
above, and Deliverator (which links it) may be relinked against a modified version
of the library. Two of the linked sub-packages — `crypto/keccak` and
`crypto/secp256k1` — carry their own **BSD-3-Clause** license.

---

## Dependencies by license

### LGPL-3.0
| Module | Version | Source |
|---|---|---|
| github.com/ethereum/go-ethereum | v1.17.4 | https://github.com/ethereum/go-ethereum (library packages — see note above) |

### Apache-2.0
| Module | Version | Source |
|---|---|---|
| github.com/consensys/gnark-crypto | v0.19.2 | https://github.com/consensys/gnark-crypto/blob/v0.19.2/LICENSE |
| github.com/crate-crypto/go-eth-kzg | v1.5.0 | https://github.com/crate-crypto/go-eth-kzg/blob/v1.5.0/LICENSE |
| github.com/spf13/cobra | v1.10.2 | https://github.com/spf13/cobra/blob/v1.10.2/LICENSE.txt |

### BSD-2-Clause
| Module | Version | Source |
|---|---|---|
| github.com/gorilla/websocket | v1.5.3 | https://github.com/gorilla/websocket/blob/v1.5.3/LICENSE |
| github.com/vmihailenco/msgpack/v5 | v5.4.1 | https://github.com/vmihailenco/msgpack/blob/v5.4.1/LICENSE |
| github.com/vmihailenco/tagparser/v2 | v2.0.0 | https://github.com/vmihailenco/tagparser/blob/v2.0.0/LICENSE |

### BSD-3-Clause
| Module | Version | Source |
|---|---|---|
| github.com/atotto/clipboard | v0.1.4 | https://github.com/atotto/clipboard/blob/v0.1.4/LICENSE |
| github.com/bits-and-blooms/bitset | v1.24.4 | https://github.com/bits-and-blooms/bitset/blob/v1.24.4/LICENSE |
| github.com/ethereum/go-ethereum/crypto/keccak | v1.17.4 | https://github.com/ethereum/go-ethereum/blob/v1.17.4/crypto/keccak/LICENSE |
| github.com/ethereum/go-ethereum/crypto/secp256k1 | v1.17.4 | https://github.com/ethereum/go-ethereum/blob/v1.17.4/crypto/secp256k1/LICENSE |
| github.com/holiman/uint256 | v1.3.2 | https://github.com/holiman/uint256/blob/v1.3.2/COPYING |
| github.com/spf13/pflag | v1.0.10 | https://github.com/spf13/pflag/blob/v1.0.10/LICENSE |
| golang.org/x/sync/errgroup | v0.19.0 | https://cs.opensource.google/go/x/sync/+/v0.19.0:LICENSE |
| golang.org/x/sys | v0.46.0 | https://cs.opensource.google/go/x/sys/+/v0.46.0:LICENSE |
| golang.org/x/term | v0.44.0 | https://cs.opensource.google/go/x/term/+/v0.44.0:LICENSE |

### MIT
| Module | Version | Source |
|---|---|---|
| github.com/BurntSushi/toml | v1.6.0 | https://github.com/BurntSushi/toml/blob/v1.6.0/COPYING |
| github.com/aymanbagabas/go-osc52/v2 | v2.0.1 | https://github.com/aymanbagabas/go-osc52/blob/v2.0.1/LICENSE |
| github.com/charmbracelet/bubbles | v1.0.0 | https://github.com/charmbracelet/bubbles/blob/v1.0.0/LICENSE |
| github.com/charmbracelet/bubbletea | v1.3.10 | https://github.com/charmbracelet/bubbletea/blob/v1.3.10/LICENSE |
| github.com/charmbracelet/colorprofile | v0.4.1 | https://github.com/charmbracelet/colorprofile/blob/v0.4.1/LICENSE |
| github.com/charmbracelet/lipgloss | v1.1.0 | https://github.com/charmbracelet/lipgloss/blob/v1.1.0/LICENSE |
| github.com/charmbracelet/x/ansi | ansi | https://github.com/charmbracelet/x/blob/ansi/v0.11.6/ansi/LICENSE |
| github.com/charmbracelet/x/cellbuf | cellbuf | https://github.com/charmbracelet/x/blob/cellbuf/v0.0.15/cellbuf/LICENSE |
| github.com/charmbracelet/x/term | term | https://github.com/charmbracelet/x/blob/term/v0.2.2/term/LICENSE |
| github.com/clipperhouse/displaywidth | v0.9.0 | https://github.com/clipperhouse/displaywidth/blob/v0.9.0/LICENSE |
| github.com/clipperhouse/stringish | v0.1.1 | https://github.com/clipperhouse/stringish/blob/v0.1.1/LICENSE |
| github.com/clipperhouse/uax29/v2/graphemes | v2.5.0 | https://github.com/clipperhouse/uax29/blob/v2.5.0/LICENSE |
| github.com/lucasb-eyer/go-colorful | v1.3.0 | https://github.com/lucasb-eyer/go-colorful/blob/v1.3.0/LICENSE |
| github.com/mattn/go-isatty | v0.0.20 | https://github.com/mattn/go-isatty/blob/v0.0.20/LICENSE |
| github.com/mattn/go-runewidth | v0.0.19 | https://github.com/mattn/go-runewidth/blob/v0.0.19/LICENSE |
| github.com/muesli/ansi | 276c6243b2f6 | https://github.com/muesli/ansi/blob/276c6243b2f6/LICENSE |
| github.com/muesli/cancelreader | v0.2.2 | https://github.com/muesli/cancelreader/blob/v0.2.2/LICENSE |
| github.com/muesli/termenv | v0.16.0 | https://github.com/muesli/termenv/blob/v0.16.0/LICENSE |
| github.com/rivo/uniseg | v0.4.7 | https://github.com/rivo/uniseg/blob/v0.4.7/LICENSE.txt |
| github.com/shopspring/decimal | v1.4.0 | https://github.com/shopspring/decimal/blob/v1.4.0/LICENSE |
| github.com/xo/terminfo | abceb7e1c41e | https://github.com/xo/terminfo/blob/abceb7e1c41e/LICENSE |
| github.com/zalando/go-keyring | v0.2.8 | https://github.com/zalando/go-keyring/blob/v0.2.8/LICENSE |
| github.com/zalando/go-keyring/internal/shellescape | v0.2.8 | https://github.com/zalando/go-keyring/blob/v0.2.8/internal/shellescape/LICENSE |
