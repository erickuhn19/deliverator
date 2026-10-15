package cmd

import (
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/spf13/cobra"

	"github.com/erickuhn19/deliverator/internal/config"
	"github.com/erickuhn19/deliverator/internal/funding"
	"github.com/erickuhn19/deliverator/internal/output"
)

var depositInfoAddress string

var depositInfoCmd = &cobra.Command{
	Use:   "deposit-info",
	Short: "How to get USDC into Hyperliquid and Papertrade (read-only, sends nothing)",
	Long: `deposit-info prints where to send USDC and in what order. It never signs or
sends anything.

Hyperliquid: send native USDC on Arbitrum from your own wallet to the
Hyperliquid bridge (minimum 5 USDC; the SENDER is credited; you need Arbitrum
ETH for gas). Or withdraw USDC from an exchange on the Hyperliquid network
straight to your Hyperliquid address. Never send to the bridge from an exchange.

Papertrade: send HyperCore USDC from your Hyperliquid account to your Papertrade
deposit proxy (minimum 10 USDC), then run 'papertrade deposit register'.

The bridge and token addresses follow --network (mainnet: Arbitrum One;
testnet: Arbitrum Sepolia). Papertrade is production only.`,
	RunE: runDepositInfo,
}

func init() {
	rootCmd.AddCommand(depositInfoCmd)
	depositInfoCmd.Flags().StringVar(&depositInfoAddress, "address", "", "your Hyperliquid address (default: wallet.master_address)")
}

func runDepositInfo(cmd *cobra.Command, args []string) error {
	const name = "deposit-info"
	network := config.NetworkMainnet
	if Cfg != nil && Cfg.Network != "" {
		network = Cfg.Network
	}
	var warnings []string
	user := ""
	s := strings.TrimSpace(depositInfoAddress)
	if s == "" && Cfg != nil {
		s = strings.TrimSpace(Cfg.Wallet.MasterAddress)
	}
	switch {
	case s == "":
		warnings = append(warnings, "no address: set wallet.master_address or pass --address to see your exchange-withdrawal destination and Papertrade deposit proxy")
	case !common.IsHexAddress(s):
		return fail(name, output.Validation("address", "--address must be a 0x wallet address"))
	default:
		user = strings.ToLower(common.HexToAddress(s).Hex())
	}

	proxy := ""
	if user != "" {
		ctx, cancel := cmdCtx()
		defer cancel()
		p, err := paperClient().DepositProxy(ctx, common.HexToAddress(user))
		if err != nil {
			warnings = append(warnings, "could not read the Papertrade deposit proxy: "+err.Error())
		} else {
			proxy = strings.ToLower(p.Hex())
		}
	}
	info, ok := funding.Build(network, user, proxy)
	if !ok {
		return fail(name, output.Validation("network", "unknown network "+network+": use mainnet or testnet"))
	}
	if network != config.NetworkMainnet {
		warnings = append(warnings, "testnet bridge and token shown. Papertrade has no testnet; its deposit proxy is production HyperEVM.")
	}
	emitWarn(name, info, warnings...)
	return nil
}
