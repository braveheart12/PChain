package tendermint

import (
	"os"
	"os/user"
	"path"
	"path/filepath"
	"runtime"
	"strings"

	"gopkg.in/urfave/cli.v1"

	tmcfg "github.com/ethereum/go-ethereum/consensus/tendermint/config/tendermint"
	cfg "github.com/tendermint/go-config"
)

func GetTendermintConfig(chainId string, ctx *cli.Context) cfg.Config {
	datadir := ctx.GlobalString(DataDirFlag.Name)
	config := tmcfg.GetConfig(datadir, chainId)

	checkAndSet(config, ctx, "moniker")
	checkAndSet(config, ctx, "node_laddr")
	checkAndSet(config, ctx, "seeds")
	checkAndSet(config, ctx, "fast_sync")
	checkAndSet(config, ctx, "skip_upnp")
	checkAndSet(config, ctx, "rpc_laddr")

	return config
}

func checkAndSet(config cfg.Config, ctx *cli.Context, opName string) {
	if ctx.GlobalIsSet(opName) {
		config.Set(opName, ctx.GlobalString(opName))

	}
}

func expandPath(p string) string {
	if strings.HasPrefix(p, "~/") || strings.HasPrefix(p, "~\\") {
		if user, err := user.Current(); err == nil {
			p = user.HomeDir + p[1:]
		}
	}
	return path.Clean(os.ExpandEnv(p))
}

func HomeDir() string {
	if home := os.Getenv("HOME"); home != "" {
		return home
	}
	if usr, err := user.Current(); err == nil {
		return usr.HomeDir
	}
	return ""
}

func DefaultDataDir() string {
	// Try to place the data folder in the user's home dir
	home := HomeDir()
	if home != "" {
		if runtime.GOOS == "darwin" {
			return filepath.Join(home, "Library", "Ethermint")
		} else if runtime.GOOS == "windows" {
			return filepath.Join(home, "AppData", "Roaming", "Ethermint")
		} else {
			return filepath.Join(home, ".ethermint")
		}
	}
	// As we cannot guess a stable location, return empty and handle later
	return ""
}
