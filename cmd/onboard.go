package cmd

import (
	"bufio"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/erickuhn19/deliverator/internal/config"
	"github.com/erickuhn19/deliverator/internal/output"
	"github.com/erickuhn19/deliverator/internal/wallet"
)

// referralJoinURL onboards new users through the Deliverator referral: they get a
// fee discount on Hyperliquid and the operator earns referral rewards. The code
// is applied by Hyperliquid when the account is created via this link.
const referralJoinURL = "https://app.hyperliquid.xyz/join/DELIVERATOR"

var onboardMaster string

var onboardAddrRe = regexp.MustCompile(`^0x[0-9a-fA-F]{40}$`)

// onboardCmd is the one-time, human-facing setup: it walks a new user through
// creating an account (with the referral discount), then takes their API wallet
// key + main address and configures Deliverator so they can trade immediately.
var onboardCmd = &cobra.Command{
	Use:   "onboard",
	Short: "Guided one-time setup: create an account, paste your API key, and trade",
	Long: "Walks you through creating a Hyperliquid account (with the Deliverator\n" +
		"referral fee discount), then stores your API wallet key in the OS keychain\n" +
		"and points Deliverator at your account — so you can start trading immediately.",
	RunE: func(cmd *cobra.Command, args []string) error {
		w := output.Writer()
		fmt.Fprint(w, "\n  Deliverator setup\n  -----------------\n\n")
		fmt.Fprintf(w, "  1. Create your Hyperliquid account (4%% off trading fees):\n       %s\n\n", referralJoinURL)
		fmt.Fprint(w, "  2. Deposit USDC into that account.\n\n")
		fmt.Fprint(w, "  3. In the Hyperliquid UI: More > API, create a wallet named \"deliverator\",\n")
		fmt.Fprint(w, "     click Authorize, and copy its address + private key.\n\n")
		fmt.Fprint(w, "  4. Paste your details below.\n\n")

		reader := bufio.NewReader(os.Stdin)

		master := strings.TrimSpace(onboardMaster)
		if master == "" {
			fmt.Fprint(w, "  Your MAIN (deposit) wallet address: ")
			line, _ := reader.ReadString('\n')
			master = strings.TrimSpace(line)
		}
		if !onboardAddrRe.MatchString(master) {
			return fail("onboard", output.Validation("bad_master", "that is not a 0x + 40-hex address"))
		}

		fmt.Fprint(w, "  Your API wallet PRIVATE KEY (input hidden): ")
		raw, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(w)
		if err != nil {
			// Not a TTY (e.g. piped): fall back to a normal line read.
			line, lerr := reader.ReadString('\n')
			if lerr != nil {
				return fail("onboard", output.Auth("read_key", "could not read the private key"))
			}
			raw = []byte(line)
		}
		keyHex := strings.TrimSpace(string(raw))
		// Best-effort wipe of the raw key bytes now that the value is extracted.
		// The keyHex string itself can't be zeroed (Go strings are immutable), but
		// the keychain is the source of truth once stored — this just shrinks the
		// window the secret lingers in a mutable buffer (audit #91 / T3-zeroize).
		for i := range raw {
			raw[i] = 0
		}
		if keyHex == "" {
			return fail("onboard", output.Validation("empty_key", "no private key entered"))
		}

		// Store the API wallet key in the OS keychain (never written to config).
		account := flagAccount
		ag, err := wallet.Store(account, keyHex)
		if err != nil {
			return fail("onboard", output.Auth("store_key", "could not store the key: "+err.Error()))
		}

		// Persist master + keychain source on a FRESHLY-loaded config so a transient
		// global flag isn't written through. Onboarding a real (referral) account
		// defaults to mainnet; --network testnet overrides.
		net := config.NetworkMainnet
		if flagNetwork != "" {
			net = flagNetwork
		}
		fresh, err := config.Load(flagConfig)
		if err != nil {
			return fail("onboard", output.Validation("config", err.Error()))
		}
		fresh.Network = net
		fresh.Wallet.MasterAddress = master
		if err := fresh.Save(config.Path()); err != nil {
			return fail("onboard", output.Unknown("save", err.Error()))
		}
		// Point the in-memory config at the new setup so the verify below uses it.
		Cfg.Network = net
		Cfg.Wallet.MasterAddress = master

		fmt.Fprintf(w, "\n  [ok] Stored API wallet %s in the keychain (account %q).\n", ag.Address, account)
		fmt.Fprintf(w, "  [ok] Config -> network %s, account %s.\n", net, master)
		fmt.Fprintf(w, "       Confirm %s matches the API wallet you authorized in the HL UI.\n\n", ag.Address)

		// Verify the account is reachable + funded (a read; the agent isn't needed
		// for this, but a successful balance read confirms the setup is coherent).
		fmt.Fprint(w, "  Verifying... ")
		ctx, cancel := cmdCtx()
		defer cancel()
		c, err := newClient(ctx)
		if err != nil {
			fmt.Fprintln(w, "could not reach Hyperliquid.")
			return fail("onboard", err)
		}
		if bal, berr := c.Balance(ctx); berr == nil && bal.AvailableCollateral != "" {
			fmt.Fprintf(w, "connected. Available collateral: $%s\n", bal.AvailableCollateral)
		} else {
			fmt.Fprintf(w, "connected (%d markets). Deposit USDC if you haven't yet.\n", len(c.Meta().Markets()))
		}
		fmt.Fprint(w, "\n  You're set up, with the referral fee discount applied. Try:  deliverator portfolio\n\n")
		fmt.Fprint(w, "  Deliverator is free and open-source. If it's useful, you can fund development by\n")
		fmt.Fprint(w, "  approving its 0.05% builder fee — a one-time signature with your MAIN wallet (the\n")
		fmt.Fprint(w, "  agent key here can't). Until you do, orders simply trade fee-free. See `deliverator\n")
		fmt.Fprint(w, "  builder status`.\n\n")
		return nil
	},
}

func init() {
	onboardCmd.Flags().StringVar(&onboardMaster, "master", "", "your main/deposit wallet address (else prompted)")
	rootCmd.AddCommand(onboardCmd)
}
