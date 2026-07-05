package cli

import (
	"fmt"
	"io"
	"os"

	"filippo.io/age"
	"github.com/spf13/cobra"
)

// CryptoCmd builds `ov crypto` with `encrypt` and `decrypt` subcommands.
// It uses age (a modern, CGo-free file-encryption library) to encrypt
// stdin to stdout using a passphrase-derived scrypt recipient.
//
// age's armor+passphrase flow keeps the CLI free of CGo and matches the
// design doc §4.x matrix selection (filippo.io/age).
func CryptoCmd(rt *Runtime) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "crypto",
		Short: "Encrypt or decrypt data with age",
	}
	cmd.AddCommand(cryptoEncryptCmd(rt))
	cmd.AddCommand(cryptoDecryptCmd(rt))
	return cmd
}

func cryptoEncryptCmd(rt *Runtime) *cobra.Command {
	var passphrase string
	cmd := &cobra.Command{
		Use:   "encrypt [FILE]",
		Short: "Encrypt FILE (or stdin) to stdout",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			in := rt.In()
			if len(args) == 1 {
				f, err := os.Open(args[0])
				if err != nil {
					return err
				}
				defer f.Close()
				in = f
			}
			if passphrase == "" {
				return fmt.Errorf("crypto: --passphrase is required (age passphrase mode)")
			}
			recipient, err := age.NewScryptRecipient(passphrase)
			if err != nil {
				return fmt.Errorf("crypto: build recipient: %w", err)
			}
			// age writes a binary armored blob; copy to stdout.
			w, err := age.Encrypt(rt.Out, recipient)
			if err != nil {
				return fmt.Errorf("crypto: encrypt: %w", err)
			}
			if _, err := io.Copy(w, in); err != nil {
				return fmt.Errorf("crypto: write: %w", err)
			}
			return w.Close()
		},
	}
	cmd.Flags().StringVar(&passphrase, "passphrase", "", "passphrase (required)")
	_ = cmd.MarkFlagRequired("passphrase")
	return cmd
}

func cryptoDecryptCmd(rt *Runtime) *cobra.Command {
	var passphrase string
	cmd := &cobra.Command{
		Use:   "decrypt [FILE]",
		Short: "Decrypt FILE (or stdin) to stdout",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			in := rt.In()
			if len(args) == 1 {
				f, err := os.Open(args[0])
				if err != nil {
					return err
				}
				defer f.Close()
				in = f
			}
			if passphrase == "" {
				return fmt.Errorf("crypto: --passphrase is required (age passphrase mode)")
			}
			ident, err := age.NewScryptIdentity(passphrase)
			if err != nil {
				return fmt.Errorf("crypto: build identity: %w", err)
			}
			r, err := age.Decrypt(in, ident)
			if err != nil {
				return fmt.Errorf("crypto: decrypt: %w", err)
			}
			_, err = io.Copy(rt.Out, r)
			return err
		},
	}
	cmd.Flags().StringVar(&passphrase, "passphrase", "", "passphrase (required)")
	_ = cmd.MarkFlagRequired("passphrase")
	return cmd
}
