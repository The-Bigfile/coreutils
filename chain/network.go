package chain

import (
	"time"

	"go.thebigfile.com/core/consensus"
	"go.thebigfile.com/core/types"
)

func parseAddr(s string) types.Address {
	addr, err := types.ParseAddress(s)
	if err != nil {
		panic(err)
	}
	return addr
}

// Mainnet returns the network parameters and genesis block for the mainnet Sia
// blockchain.
func Mainnet() (*consensus.Network, types.Block) {
	n := &consensus.Network{
		Name: "mainnet",

		InitialCoinbase: types.BigFiles(300000),
		MinimumCoinbase: types.BigFiles(30000),
		InitialTarget:   types.BlockID{4: 32},
		BlockInterval:   10 * time.Minute,
		MaturityDelay:   144,
	}
	n.HardforkDevAddr.Height = 10000
	n.HardforkDevAddr.OldAddress = parseAddr("8320172b2ec599be4467e3999d0c8af57d30dde39ef5a61a66a86d809e7595f72a14834f5447")
	n.HardforkDevAddr.NewAddress = parseAddr("8320172b2ec599be4467e3999d0c8af57d30dde39ef5a61a66a86d809e7595f72a14834f5447")

	n.HardforkTax.Height = 21000

	n.HardforkStorageProof.Height = 100000

	n.HardforkOak.Height = 135000
	n.HardforkOak.FixHeight = 139000
	n.HardforkOak.GenesisTimestamp = time.Unix(1433600000, 0) // June 6th, 2015 @ 2:13pm UTC

	n.HardforkASIC.Height = 179000
	n.HardforkASIC.OakTime = 120000 * time.Second
	n.HardforkASIC.OakTarget = types.BlockID{8: 32}

	n.HardforkFoundation.Height = 298000
	n.HardforkFoundation.PrimaryAddress = parseAddr("8320172b2ec599be4467e3999d0c8af57d30dde39ef5a61a66a86d809e7595f72a14834f5447")
	n.HardforkFoundation.FailsafeAddress = parseAddr("8320172b2ec599be4467e3999d0c8af57d30dde39ef5a61a66a86d809e7595f72a14834f5447")

	n.HardforkV2.AllowHeight = 526000   // June 6th, 2025 @ 6:00am UTC
	n.HardforkV2.RequireHeight = 530000 // July 4th, 2025 @ 2:00am UTC

	b := types.Block{
		Timestamp: n.HardforkOak.GenesisTimestamp,
		Transactions: []types.Transaction{{
			SiafundOutputs: []types.SiafundOutput{
				{Address: parseAddr("8320172b2ec599be4467e3999d0c8af57d30dde39ef5a61a66a86d809e7595f72a14834f5447"), Value: 10000},
				},
		}},
	}

	return n, b
}

// TestnetZen returns the chain parameters and genesis block for the "Zen"
// testnet chain.
func TestnetZen() (*consensus.Network, types.Block) {
	n := &consensus.Network{
		Name: "zen",

		InitialCoinbase: types.BigFiles(300000),
		MinimumCoinbase: types.BigFiles(300000),
		InitialTarget:   types.BlockID{3: 1},
		BlockInterval:   10 * time.Minute,
		MaturityDelay:   144,
	}

	n.HardforkDevAddr.Height = 1
	n.HardforkDevAddr.OldAddress = types.Address{}
	n.HardforkDevAddr.NewAddress = types.Address{}

	n.HardforkTax.Height = 2

	n.HardforkStorageProof.Height = 5

	n.HardforkOak.Height = 10
	n.HardforkOak.FixHeight = 12
	n.HardforkOak.GenesisTimestamp = time.Unix(1673600000, 0) // January 13, 2023 @ 08:53 GMT

	n.HardforkASIC.Height = 20
	n.HardforkASIC.OakTime = 10000 * time.Second
	n.HardforkASIC.OakTarget = types.BlockID{3: 1}

	n.HardforkFoundation.Height = 30
	n.HardforkFoundation.PrimaryAddress = parseAddr("8320172b2ec599be4467e3999d0c8af57d30dde39ef5a61a66a86d809e7595f72a14834f5447")
	n.HardforkFoundation.FailsafeAddress = types.VoidAddress

	n.HardforkV2.AllowHeight = 112000   // March 1, 2025 @ 7:00:00 UTC
	n.HardforkV2.RequireHeight = 114000 // ~ 2 weeks later

	b := types.Block{
		Timestamp: n.HardforkOak.GenesisTimestamp,
		Transactions: []types.Transaction{{
			BigFileOutputs: []types.BigFileOutput{{
				Address: parseAddr("8320172b2ec599be4467e3999d0c8af57d30dde39ef5a61a66a86d809e7595f72a14834f5447"),
				Value:   types.BigFiles(1).Mul64(1e12),
			}},
			SiafundOutputs: []types.SiafundOutput{{
				Address: parseAddr("8320172b2ec599be4467e3999d0c8af57d30dde39ef5a61a66a86d809e7595f72a14834f5447"),
				Value:   10000,
			}},
		}},
	}

	return n, b
}

// TestnetAnagami returns the chain parameters and genesis block for the "Anagami"
// testnet chain.
func TestnetAnagami() (*consensus.Network, types.Block) {
	// use a modified version of Zen
	n, genesis := TestnetZen()

	n.Name = "anagami"
	n.HardforkOak.GenesisTimestamp = time.Date(2024, time.August, 22, 0, 0, 0, 0, time.UTC)
	n.HardforkV2.AllowHeight = 2016         // ~2 weeks in
	n.HardforkV2.RequireHeight = 2016 + 288 // ~2 days later

	n.HardforkFoundation.PrimaryAddress = parseAddr("8320172b2ec599be4467e3999d0c8af57d30dde39ef5a61a66a86d809e7595f72a14834f5447")
	n.HardforkFoundation.FailsafeAddress = types.VoidAddress

	// move the genesis airdrops for easier testing
	genesis.Transactions[0].BigFileOutputs = []types.BigFileOutput{{
		Address: parseAddr("8320172b2ec599be4467e3999d0c8af57d30dde39ef5a61a66a86d809e7595f72a14834f5447"),
		Value:   types.BigFiles(1).Mul64(1e12),
	}}
	genesis.Transactions[0].SiafundOutputs = []types.SiafundOutput{{
		Address: parseAddr("8320172b2ec599be4467e3999d0c8af57d30dde39ef5a61a66a86d809e7595f72a14834f5447"),
		Value:   10000,
	}}
	return n, genesis
}

// TestnetErravimus returns the chain parameters and genesis block for the "Erravimus"
// testnet chain.
func TestnetErravimus() (*consensus.Network, types.Block) {
	// use a modified version of Zen
	n, genesis := TestnetZen()

	n.Name = "erravimus"
	n.HardforkOak.GenesisTimestamp = time.Date(2025, time.March, 18, 0, 0, 0, 0, time.UTC)
	n.HardforkV2.AllowHeight = 2016                              // ~2 weeks in
	n.HardforkV2.RequireHeight = n.HardforkV2.AllowHeight + 1440 // ~10 days later

	n.HardforkFoundation.PrimaryAddress = parseAddr("8320172b2ec599be4467e3999d0c8af57d30dde39ef5a61a66a86d809e7595f72a14834f5447")
	n.HardforkFoundation.FailsafeAddress = types.VoidAddress

	// move the genesis airdrops for easier testing
	genesis.Transactions[0].BigFileOutputs = []types.BigFileOutput{{
		Address: parseAddr("8320172b2ec599be4467e3999d0c8af57d30dde39ef5a61a66a86d809e7595f72a14834f5447"),
		Value:   types.BigFiles(1).Mul64(1e12),
	}}
	genesis.Transactions[0].SiafundOutputs = []types.SiafundOutput{{
		Address: parseAddr("8320172b2ec599be4467e3999d0c8af57d30dde39ef5a61a66a86d809e7595f72a14834f5447"),
		Value:   10000,
	}}
	return n, genesis
}
