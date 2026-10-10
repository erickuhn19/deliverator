package papertrade

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"os"
	"path/filepath"
	"strings"

	"github.com/ethereum/go-ethereum/crypto"

	"github.com/erickuhn19/deliverator/internal/state"
)

// NonceKey is the venue client's storage key. route is "session" for trading
// and registration, "paper" for staking. Nonces are per (chain, contract,
// route, user) and are NOT an on-chain counter. Reusing one inside a new
// signature opens a second position; the store hands each out once.
func NonceKey(verifyingContract, route, user string) string {
	return strings.Join([]string{
		"papertrade.clientNonce.v1",
		"999",
		strings.ToLower(verifyingContract),
		route,
		strings.ToLower(user),
	}, ":")
}

// NonceStore persists the next unused nonce. The initial value matches the
// venue client: a random 24-bit integer (or 1) multiplied by 256, then +1
// after each take.
type NonceStore struct{ dir string }

func NewNonceStore(dir string) *NonceStore { return &NonceStore{dir: dir} }

type nonceFile struct {
	Key  string `json:"key"`
	Next string `json:"nextNonce"`
}

// Take returns the next nonce and persists the one after it before returning.
// A crash after Take and before the relayer accepts the intent burns the nonce.
// That is the safe direction: a burned nonce cannot double-fill.
func (s *NonceStore) Take(key string) (string, error) {
	if s == nil || s.dir == "" || key == "" {
		return "", errString("nonce store is not configured")
	}
	id := hex.EncodeToString(crypto.Keccak256([]byte(key)))
	lockPath := filepath.Join(s.dir, id+".lock")
	dataPath := filepath.Join(s.dir, id+".json")
	lk, err := state.Lock(lockPath)
	if err != nil {
		return "", err
	}
	defer lk.Unlock()

	cur := (*big.Int)(nil)
	if b, err := os.ReadFile(dataPath); err == nil {
		var f nonceFile
		if json.Unmarshal(b, &f) == nil && f.Key == key {
			if n, ok := new(big.Int).SetString(strings.TrimSpace(f.Next), 10); ok && n.Sign() > 0 {
				cur = n
			}
		}
	}
	if cur == nil {
		cur, err = freshNonce()
		if err != nil {
			return "", err
		}
	}
	next := new(big.Int).Add(cur, big.NewInt(1))
	payload, err := json.Marshal(nonceFile{Key: key, Next: next.String()})
	if err != nil {
		return "", err
	}
	if err := state.WriteFileAtomic(dataPath, append(payload, '\n'), 0o600); err != nil {
		return "", err
	}
	return cur.String(), nil
}

func freshNonce() (*big.Int, error) {
	var buf [3]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return nil, err
	}
	n := new(big.Int).SetBytes(buf[:])
	if n.Sign() == 0 {
		n = big.NewInt(1)
	}
	return n.Mul(n, big.NewInt(256)), nil
}
