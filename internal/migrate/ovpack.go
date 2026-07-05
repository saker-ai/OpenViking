package migrate

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/saker-ai/ctxhub/pkg/ovpack"
)

// ovpackCmd is the parent for ovpack pack/unpack subcommands.
func ovpackCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ovpack",
		Short: "Pack and unpack ovpack offline archives",
	}
	cmd.AddCommand(ovpackPackCmd(), ovpackUnpackCmd())
	return cmd
}

// ovpackPackCmd implements `ovpack pack <dir>`.
func ovpackPackCmd() *cobra.Command {
	var (
		outPath string
		baseURI string
		account string
	)
	cmd := &cobra.Command{
		Use:   "pack <dir>",
		Short: "Pack a directory into an ovpack archive",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := args[0]
			if outPath == "" {
				outPath = strings.TrimSuffix(filepath.Clean(dir), string(filepath.Separator)) + ".ovpack"
			}
			if err := packDir(dir, outPath, account, baseURI); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "wrote %s\n", outPath)
			return nil
		},
	}
	cmd.Flags().StringVarP(&outPath, "output", "o", "", "output pack path (default <dir>.ovpack)")
	cmd.Flags().StringVar(&baseURI, "base-uri", "", "manifest source.base_uri (e.g. viking://agent/)")
	cmd.Flags().StringVar(&account, "account", "", "manifest source.account")
	return cmd
}

// ovpackUnpackCmd implements `ovpack unpack <pack> <dir>`.
func ovpackUnpackCmd() *cobra.Command {
	var skipVerify bool
	cmd := &cobra.Command{
		Use:   "unpack <pack> <dir>",
		Short: "Unpack an ovpack archive into a directory",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			packPath, dir := args[0], args[1]
			if err := unpackDir(packPath, dir, skipVerify); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "unpacked to %s\n", dir)
			return nil
		},
	}
	cmd.Flags().BoolVar(&skipVerify, "skip-verify", false, "skip checksum verification (dangerous)")
	return cmd
}

// packDir walks dir and writes every file (except manifest.json, which
// is regenerated) into a new ovpack at outPath.
func packDir(dir, outPath, account, baseURI string) error {
	if _, err := os.Stat(dir); err != nil {
		return fmt.Errorf("ovpack pack: %w", err)
	}
	f, err := os.Create(outPath)
	if err != nil {
		return fmt.Errorf("ovpack pack: %w", err)
	}
	defer f.Close()
	w := ovpack.NewWriter(f)
	if account != "" || baseURI != "" {
		w.SetSource(account, baseURI)
	}
	walkErr := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == "manifest.json" {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return w.WriteFile(rel, data)
	})
	if walkErr != nil {
		f.Close()
		os.Remove(outPath)
		return fmt.Errorf("ovpack pack: %w", walkErr)
	}
	if err := w.Close(); err != nil {
		f.Close()
		os.Remove(outPath)
		return fmt.Errorf("ovpack pack: %w", err)
	}
	return f.Close()
}

// unpackDir reads packPath, optionally verifies checksums, and extracts
// every member to dir.
func unpackDir(packPath, dir string, skipVerify bool) error {
	f, err := os.Open(packPath)
	if err != nil {
		return fmt.Errorf("ovpack unpack: %w", err)
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		return fmt.Errorf("ovpack unpack: %w", err)
	}
	r, err := ovpack.Open(f, stat.Size())
	if err != nil {
		return fmt.Errorf("ovpack unpack: %w", err)
	}
	if !skipVerify {
		if err := r.Verify(); err != nil {
			if errors.Is(err, ovpack.ErrChecksumMismatch) {
				return fmt.Errorf("ovpack unpack: checksum verification failed: %w", err)
			}
			return fmt.Errorf("ovpack unpack: %w", err)
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("ovpack unpack: %w", err)
	}
	if err := r.ExtractTo(dir); err != nil {
		return fmt.Errorf("ovpack unpack: %w", err)
	}
	return nil
}
