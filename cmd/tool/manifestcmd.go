package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/unidoc/alpine-zfsboot/internal/payloadsum"
)

// writeManifestFiles writes CHECKSUM and BLKSUM for a kernel/initrd pair
// into dir (BLKSUM first, CHECKSUM last - the same order install/update
// use). For image builds: iso.sh puts them on the ISO's FAT image, so the
// BIOS ISO boot checks its payload too.
func writeManifestFiles(dir string, kernel, initrd []byte, mode string) error {
	sum, err := payloadsum.Generate(kernel, initrd, mode)
	if err != nil {
		return err
	}
	blk, err := payloadsum.GenerateBlkSum(kernel, initrd)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "BLKSUM"), blk, 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "CHECKSUM"), sum, 0o644)
}

func newPayloadManifestCmd() *cobra.Command {
	var kernelFile, initrdFile, outDir, mode string
	cmd := &cobra.Command{
		Use:    "payload-manifest",
		Short:  "write CHECKSUM and BLKSUM for a kernel/initrd pair into a directory (image builds: iso.sh/build.sh)",
		Hidden: true,
		Args:   cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			kernel, err := os.ReadFile(kernelFile)
			die(err)
			initrd, err := os.ReadFile(initrdFile)
			die(err)
			die(writeManifestFiles(outDir, kernel, initrd, mode))
			fmt.Printf("%s: CHECKSUM (integrity mode %s) and BLKSUM written\n", outDir, mode)
		},
	}
	cmd.Flags().StringVar(&kernelFile, "kernel", "", "the bzImage")
	cmd.Flags().StringVar(&initrdFile, "initrd", "", "the initrd")
	cmd.Flags().StringVar(&outDir, "out", "", "directory to write CHECKSUM and BLKSUM into")
	cmd.Flags().StringVar(&mode, "integrity", payloadsum.ModeWarn, "integrity mode recorded in CHECKSUM (off, warn, enforce)")
	cmd.MarkFlagRequired("kernel")
	cmd.MarkFlagRequired("initrd")
	cmd.MarkFlagRequired("out")
	return cmd
}
