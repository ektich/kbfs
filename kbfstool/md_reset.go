package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/keybase/kbfs/libkbfs"
	"golang.org/x/net/context"
)

func mdResetOne(
	ctx context.Context, config libkbfs.Config, tlfPath string,
	checkValid, dryRun, force bool) error {
	handle, err := parseTLFPath(ctx, config.KBPKI(), tlfPath)
	if err != nil {
		return err
	}

	// This function is loosely adapted from
	// folderBranchOps.initMDLocked.

	username, uid, err := config.KBPKI().GetCurrentUserInfo(ctx)
	if err != nil {
		return err
	}

	// Make sure we're a writer before doing anything else.
	if !handle.IsWriter(uid) {
		return libkbfs.NewWriteAccessError(
			handle, username, handle.GetCanonicalPath())
	}

	fmt.Printf("Looking for unmerged branch...\n")

	_, unmergedIRMD, err := config.MDOps().GetForHandle(
		ctx, handle, libkbfs.Unmerged)
	if err != nil {
		return err
	}
	if unmergedIRMD != (libkbfs.ImmutableRootMetadata{}) {
		return fmt.Errorf(
			"%s has unmerged data; try unstaging it first",
			tlfPath)
	}

	fmt.Printf("Getting latest metadata...\n")

	_, irmd, err := config.MDOps().GetForHandle(
		ctx, handle, libkbfs.Merged)
	if err != nil {
		return err
	}

	if irmd == (libkbfs.ImmutableRootMetadata{}) {
		fmt.Printf("No TLF found for %q\n", tlfPath)
		return nil
	}

	if checkValid {
		rootPtr := irmd.Data().Dir.BlockPointer
		if rootPtr.Ref().IsValid() {
			var dirBlock libkbfs.DirBlock
			err = config.BlockOps().Get(
				ctx, irmd, rootPtr, &dirBlock)
			if err == nil {
				fmt.Printf("Got no error when getting root block %s; not doing anything\n", rootPtr)
				return nil
			}
			fmt.Printf("Got error %s when getting root block %s, so revision %d is broken. Making successor...\n",
				err, rootPtr, irmd.Revision())
		} else {
			// This happens in the wild, but only for folders used
			// for journal-related testing early on.
			fmt.Printf("Root block pointer is invalid, so revision %d is broken. Making successor...\n",
				irmd.Revision())
		}
	}

	rmdNext, err := irmd.MakeSuccessor(ctx, config, irmd.MdID(), true)
	if err != nil {
		return err
	}

	// TODO: Add an option to scan for and use the last known good
	// root block.
	_, info, readyBlockData, err :=
		libkbfs.ResetRootBlock(ctx, config, uid, rmdNext)
	if err != nil {
		return err
	}

	fmt.Printf(
		"Will put an empty root block for tlfID=%s with blockInfo=%s and bufLen=%d\n",
		rmdNext.TlfID(), info, readyBlockData.GetEncodedSize())
	fmt.Print("Will put MD:\n")
	err = mdDumpOneReadOnly(ctx, config, rmdNext.ReadOnly())
	if err != nil {
		return err
	}

	if dryRun {
		fmt.Print("Dry-run set; not doing anything\n")
		return nil
	}

	if !force {
		fmt.Print("Are you sure you want to continue? [y/N]: ")
		response, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil {
			return err
		}
		response = strings.ToLower(strings.TrimSpace(response))
		if response != "y" {
			fmt.Printf("Didn't confirm; not doing anything\n")
			return nil
		}
	}

	fmt.Printf("Putting block %s...\n", info)

	err = libkbfs.PutBlockCheckQuota(
		ctx, config.BlockServer(), config.Reporter(),
		rmdNext.TlfID(), info.BlockPointer, readyBlockData,
		handle.GetCanonicalName())
	if err != nil {
		return err
	}

	// Assume there's no need to unembed the block changes.

	fmt.Printf("Putting revision %d...\n", rmdNext.Revision())

	mdID, err := config.MDOps().Put(ctx, rmdNext)
	if err != nil {
		return err
	}

	fmt.Printf("New MD has id %s\n", mdID)

	return nil
}

const mdResetUsageStr = `Usage:
  kbfstool md reset /keybase/[public|private]/user1,assertion2

`

func mdReset(ctx context.Context, config libkbfs.Config, args []string) (exitStatus int) {
	flags := flag.NewFlagSet("kbfs md reset", flag.ContinueOnError)
	checkValid := flags.Bool("c", true, "If set, don't do anything if the existing root block is valid")
	dryRun := flags.Bool("d", false, "Dry run: don't actually do anything.")
	force := flags.Bool("f", false, "If set, skip confirmation prompt.")
	err := flags.Parse(args)
	if err != nil {
		printError("md reset", err)
		return 1
	}

	inputs := flags.Args()
	if len(inputs) != 1 {
		fmt.Print(mdResetUsageStr)
		return 1
	}

	err = mdResetOne(ctx, config, inputs[0], *checkValid, *dryRun, *force)
	if err != nil {
		printError("md reset", err)
		return 1
	}

	fmt.Print("\n")

	return 0
}
