// Copyright 2016 Keybase Inc. All rights reserved.
// Use of this source code is governed by a BSD
// license that can be found in the LICENSE file.

package libkbfs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/keybase/client/go/protocol/keybase1"
	"github.com/keybase/kbfs/kbfscodec"
	"github.com/keybase/kbfs/kbfscrypto"
	"github.com/keybase/kbfs/tlf"
)

// mdServerTlfStorage stores an ordered list of metadata IDs for each
// branch of a single TLF, along with the associated metadata objects,
// in flat files on disk.
//
// The directory layout looks like:
//
// dir/md_branch_journals/00..00/EARLIEST
// dir/md_branch_journals/00..00/LATEST
// dir/md_branch_journals/00..00/0...001
// dir/md_branch_journals/00..00/0...002
// dir/md_branch_journals/00..00/0...fff
// dir/md_branch_journals/5f..3d/EARLIEST
// dir/md_branch_journals/5f..3d/LATEST
// dir/md_branch_journals/5f..3d/0...0ff
// dir/md_branch_journals/5f..3d/0...100
// dir/md_branch_journals/5f..3d/0...fff
// dir/mds/0100/0...01
// ...
// dir/mds/01ff/f...ff
// dir/wkbv3/0100...01
// ...
// dir/wkbv3/0100...ff
// dir/rkbv3/0100...01
// ...
// dir/rkbv3/0100...ff
//
// Each branch has its own subdirectory with a journal; the journal
// ordinals are just MetadataRevisions, and the journal entries are
// just MdIDs. (Branches are usually temporary, so no need to splay
// them.)
//
// The Metadata objects are stored separately in dir/mds. Each block
// has its own subdirectory with its ID as a name. The MD
// subdirectories are splayed over (# of possible hash types) * 256
// subdirectories -- one byte for the hash type (currently only one)
// plus the first byte of the hash data -- using the first four
// characters of the name to keep the number of directories in dir
// itself to a manageable number, similar to git.
//
// Writer (reader) key bundles for V3 metadata objects are stored
// separately in dir/wkbv3 (dir/rkbv3). The number of bundles is
// small, so no need to splay them.
type mdServerTlfStorage struct {
	tlfID  tlf.ID
	codec  kbfscodec.Codec
	crypto cryptoPure
	clock  Clock
	mdVer  MetadataVer
	dir    string

	// Protects any IO operations in dir or any of its children,
	// as well as branchJournals and its contents.
	lock           sync.RWMutex
	branchJournals map[BranchID]mdIDJournal
}

func makeMDServerTlfStorage(tlfID tlf.ID, codec kbfscodec.Codec,
	crypto cryptoPure, clock Clock, mdVer MetadataVer,
	dir string) *mdServerTlfStorage {
	journal := &mdServerTlfStorage{
		tlfID:          tlfID,
		codec:          codec,
		crypto:         crypto,
		clock:          clock,
		mdVer:          mdVer,
		dir:            dir,
		branchJournals: make(map[BranchID]mdIDJournal),
	}
	return journal
}

// The functions below are for building various paths.

func (s *mdServerTlfStorage) branchJournalsPath() string {
	return filepath.Join(s.dir, "md_branch_journals")
}

func (s *mdServerTlfStorage) mdsPath() string {
	return filepath.Join(s.dir, "mds")
}

func (s *mdServerTlfStorage) writerKeyBundleV3Path(
	id TLFWriterKeyBundleID) string {
	return filepath.Join(s.dir, "wkbv3", id.String())
}

func (s *mdServerTlfStorage) readerKeyBundleV3Path(
	id TLFReaderKeyBundleID) string {
	return filepath.Join(s.dir, "rkbv3", id.String())
}

func (s *mdServerTlfStorage) mdPath(id MdID) string {
	idStr := id.String()
	return filepath.Join(s.mdsPath(), idStr[:4], idStr[4:])
}

// serializedRMDS is the structure stored in mdPath(id).
type serializedRMDS struct {
	EncodedRMDS []byte
	Timestamp   time.Time
	Version     MetadataVer
}

// getMDReadLocked verifies the MD data (but not the signature) for
// the given ID and returns it.
//
// TODO: Verify signature?
func (s *mdServerTlfStorage) getMDReadLocked(id MdID) (
	*RootMetadataSigned, error) {
	// Read file.

	var srmds serializedRMDS
	err := kbfscodec.DeserializeFromFile(s.codec, s.mdPath(id), &srmds)
	if err != nil {
		return nil, err
	}

	rmds, err := DecodeRootMetadataSigned(
		s.codec, s.tlfID, srmds.Version, s.mdVer, srmds.EncodedRMDS,
		srmds.Timestamp)
	if err != nil {
		return nil, err
	}

	// Check integrity.

	mdID, err := s.crypto.MakeMdID(rmds.MD)
	if err != nil {
		return nil, err
	}

	if id != mdID {
		return nil, fmt.Errorf(
			"Metadata ID mismatch: expected %s, got %s",
			id, mdID)
	}

	return rmds, nil
}

func (s *mdServerTlfStorage) putMDLocked(
	rmds *RootMetadataSigned) (MdID, error) {
	id, err := s.crypto.MakeMdID(rmds.MD)
	if err != nil {
		return MdID{}, err
	}

	_, err = s.getMDReadLocked(id)
	if os.IsNotExist(err) {
		// Continue on.
	} else if err != nil {
		return MdID{}, err
	} else {
		// Entry exists, so nothing else to do.
		return id, nil
	}

	encodedRMDS, err := EncodeRootMetadataSigned(s.codec, rmds)
	if err != nil {
		return MdID{}, err
	}

	srmds := serializedRMDS{
		EncodedRMDS: encodedRMDS,
		Timestamp:   s.clock.Now(),
		Version:     rmds.MD.Version(),
	}

	err = kbfscodec.SerializeToFile(s.codec, srmds, s.mdPath(id))
	if err != nil {
		return MdID{}, err
	}

	return id, nil
}

func (s *mdServerTlfStorage) getOrCreateBranchJournalLocked(
	bid BranchID) (mdIDJournal, error) {
	j, ok := s.branchJournals[bid]
	if ok {
		return j, nil
	}

	dir := filepath.Join(s.branchJournalsPath(), bid.String())
	err := os.MkdirAll(dir, 0700)
	if err != nil {
		return mdIDJournal{}, err
	}

	j = makeMdIDJournal(s.codec, dir)
	s.branchJournals[bid] = j
	return j, nil
}

func (s *mdServerTlfStorage) getHeadForTLFReadLocked(bid BranchID) (
	rmds *RootMetadataSigned, err error) {
	j, ok := s.branchJournals[bid]
	if !ok {
		return nil, nil
	}
	entry, exists, err := j.getLatestEntry()
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, nil
	}
	return s.getMDReadLocked(entry.ID)
}

func (s *mdServerTlfStorage) checkGetParamsReadLocked(
	currentUID keybase1.UID, bid BranchID) error {
	mergedMasterHead, err := s.getHeadForTLFReadLocked(NullBranchID)
	if err != nil {
		return MDServerError{err}
	}

	if mergedMasterHead != nil {
		extra, err := s.getExtraMetadataReadLocked(
			mergedMasterHead.MD.GetTLFWriterKeyBundleID(),
			mergedMasterHead.MD.GetTLFReaderKeyBundleID())
		if err != nil {
			return MDServerError{err}
		}
		ok, err := isReader(currentUID, mergedMasterHead.MD, extra)
		if err != nil {
			return MDServerError{err}
		}
		if !ok {
			return MDServerErrorUnauthorized{}
		}
	}

	return nil
}

func (s *mdServerTlfStorage) getRangeReadLocked(
	currentUID keybase1.UID, bid BranchID, start, stop MetadataRevision) (
	[]*RootMetadataSigned, error) {
	err := s.checkGetParamsReadLocked(currentUID, bid)
	if err != nil {
		return nil, err
	}

	j, ok := s.branchJournals[bid]
	if !ok {
		return nil, nil
	}

	realStart, entries, err := j.getEntryRange(start, stop)
	if err != nil {
		return nil, err
	}
	var rmdses []*RootMetadataSigned
	for i, entry := range entries {
		expectedRevision := realStart + MetadataRevision(i)
		rmds, err := s.getMDReadLocked(entry.ID)
		if err != nil {
			return nil, MDServerError{err}
		}
		if expectedRevision != rmds.MD.RevisionNumber() {
			panic(fmt.Errorf("expected revision %v, got %v",
				expectedRevision, rmds.MD.RevisionNumber()))
		}
		rmdses = append(rmdses, rmds)
	}

	return rmdses, nil
}

func (s *mdServerTlfStorage) getExtraMetadataReadLocked(
	wkbID TLFWriterKeyBundleID, rkbID TLFReaderKeyBundleID) (
	ExtraMetadata, error) {
	wkb, rkb, err := s.getKeyBundlesReadLocked(wkbID, rkbID)
	if err != nil {
		return nil, err
	}
	if wkb == nil || rkb == nil {
		return nil, nil
	}
	return &ExtraMetadataV3{wkb: wkb, rkb: rkb}, nil
}

func (s *mdServerTlfStorage) getKeyBundlesReadLocked(
	wkbID TLFWriterKeyBundleID, rkbID TLFReaderKeyBundleID) (
	*TLFWriterKeyBundleV3, *TLFReaderKeyBundleV3, error) {
	if (wkbID == TLFWriterKeyBundleID{}) !=
		(rkbID == TLFReaderKeyBundleID{}) {
		return nil, nil, fmt.Errorf(
			"wkbID is empty (%t) != rkbID is empty (%t)",
			wkbID == TLFWriterKeyBundleID{},
			rkbID == TLFReaderKeyBundleID{})
	}

	if wkbID == (TLFWriterKeyBundleID{}) {
		return nil, nil, nil
	}

	var wkb TLFWriterKeyBundleV3
	err := kbfscodec.DeserializeFromFile(
		s.codec, s.writerKeyBundleV3Path(wkbID), &wkb)
	if err != nil {
		return nil, nil, err
	}

	var rkb TLFReaderKeyBundleV3
	err = kbfscodec.DeserializeFromFile(
		s.codec, s.readerKeyBundleV3Path(rkbID), &rkb)
	if err != nil {
		return nil, nil, err
	}

	err = checkKeyBundlesV3(s.crypto, wkbID, rkbID, &wkb, &rkb)
	if err != nil {
		return nil, nil, err
	}

	return &wkb, &rkb, nil
}

func checkKeyBundlesV3(
	crypto cryptoPure,
	wkbID TLFWriterKeyBundleID, rkbID TLFReaderKeyBundleID,
	wkb *TLFWriterKeyBundleV3, rkb *TLFReaderKeyBundleV3) error {
	computedWKBID, err := crypto.MakeTLFWriterKeyBundleID(wkb)
	if err != nil {
		return err
	}

	if wkbID != computedWKBID {
		return fmt.Errorf("Expected WKB ID %s, got %s",
			wkbID, computedWKBID)
	}

	computedRKBID, err := crypto.MakeTLFReaderKeyBundleID(rkb)
	if err != nil {
		return err
	}

	if rkbID != computedRKBID {
		return fmt.Errorf("Expected RKB ID %s, got %s",
			rkbID, computedRKBID)
	}

	return nil
}

func (s *mdServerTlfStorage) putExtraMetadataLocked(
	rmds *RootMetadataSigned, extra ExtraMetadata) error {
	if extra == nil {
		return nil
	}

	wkbID := rmds.MD.GetTLFWriterKeyBundleID()
	if wkbID == (TLFWriterKeyBundleID{}) {
		panic("writer key bundle ID is empty")
	}

	rkbID := rmds.MD.GetTLFReaderKeyBundleID()
	if rkbID == (TLFReaderKeyBundleID{}) {
		panic("reader key bundle ID is empty")
	}

	extraV3, ok := extra.(*ExtraMetadataV3)
	if !ok {
		return errors.New("Invalid extra metadata")
	}

	err := checkKeyBundlesV3(
		s.crypto, wkbID, rkbID, extraV3.wkb, extraV3.rkb)
	if err != nil {
		return err
	}

	err = kbfscodec.SerializeToFile(
		s.codec, extraV3.wkb, s.writerKeyBundleV3Path(wkbID))
	if err != nil {
		return err
	}

	err = kbfscodec.SerializeToFile(
		s.codec, extraV3.rkb, s.readerKeyBundleV3Path(rkbID))
	if err != nil {
		return err
	}

	return nil
}

func (s *mdServerTlfStorage) isShutdownReadLocked() bool {
	return s.branchJournals == nil
}

// All functions below are public functions.

var errMDServerTlfStorageShutdown = errors.New("mdServerTlfStorage is shutdown")

func (s *mdServerTlfStorage) journalLength(bid BranchID) (uint64, error) {
	s.lock.RLock()
	defer s.lock.RUnlock()

	if s.isShutdownReadLocked() {
		return 0, errMDServerTlfStorageShutdown
	}

	j, ok := s.branchJournals[bid]
	if !ok {
		return 0, nil
	}

	return j.length()
}

func (s *mdServerTlfStorage) getForTLF(
	currentUID keybase1.UID, bid BranchID) (*RootMetadataSigned, error) {
	s.lock.RLock()
	defer s.lock.RUnlock()

	if s.isShutdownReadLocked() {
		return nil, errMDServerTlfStorageShutdown
	}

	err := s.checkGetParamsReadLocked(currentUID, bid)
	if err != nil {
		return nil, err
	}

	rmds, err := s.getHeadForTLFReadLocked(bid)
	if err != nil {
		return nil, MDServerError{err}
	}
	return rmds, nil
}

func (s *mdServerTlfStorage) getRange(
	currentUID keybase1.UID, bid BranchID, start, stop MetadataRevision) (
	[]*RootMetadataSigned, error) {
	s.lock.RLock()
	defer s.lock.RUnlock()

	if s.isShutdownReadLocked() {
		return nil, errMDServerTlfStorageShutdown
	}

	return s.getRangeReadLocked(currentUID, bid, start, stop)
}

func (s *mdServerTlfStorage) put(
	currentUID keybase1.UID, currentVerifyingKey kbfscrypto.VerifyingKey,
	rmds *RootMetadataSigned, extra ExtraMetadata) (
	recordBranchID bool, err error) {
	s.lock.Lock()
	defer s.lock.Unlock()

	if s.isShutdownReadLocked() {
		return false, errMDServerTlfStorageShutdown
	}

	if extra == nil {
		var err error
		extra, err = s.getExtraMetadataReadLocked(
			rmds.MD.GetTLFWriterKeyBundleID(),
			rmds.MD.GetTLFReaderKeyBundleID())
		if err != nil {
			return false, MDServerError{err}
		}
	}

	err = rmds.IsValidAndSigned(s.codec, s.crypto, extra)
	if err != nil {
		return false, MDServerErrorBadRequest{Reason: err.Error()}
	}

	err = rmds.IsLastModifiedBy(currentUID, currentVerifyingKey)
	if err != nil {
		return false, MDServerErrorBadRequest{Reason: err.Error()}
	}

	// Check permissions

	mergedMasterHead, err := s.getHeadForTLFReadLocked(NullBranchID)
	if err != nil {
		return false, MDServerError{err}
	}

	// TODO: Figure out nil case.
	if mergedMasterHead != nil {
		prevExtra, err := s.getExtraMetadataReadLocked(
			mergedMasterHead.MD.GetTLFWriterKeyBundleID(),
			mergedMasterHead.MD.GetTLFReaderKeyBundleID())
		if err != nil {
			return false, MDServerError{err}
		}
		ok, err := isWriterOrValidRekey(
			s.codec, currentUID,
			mergedMasterHead.MD, rmds.MD,
			prevExtra, extra)
		if err != nil {
			return false, MDServerError{err}
		}
		if !ok {
			return false, MDServerErrorUnauthorized{}
		}
	}

	bid := rmds.MD.BID()
	mStatus := rmds.MD.MergedStatus()

	head, err := s.getHeadForTLFReadLocked(bid)
	if err != nil {
		return false, MDServerError{err}
	}

	if mStatus == Unmerged && head == nil {
		// currHead for unmerged history might be on the main branch
		prevRev := rmds.MD.RevisionNumber() - 1
		rmdses, err := s.getRangeReadLocked(
			currentUID, NullBranchID, prevRev, prevRev)
		if err != nil {
			return false, MDServerError{err}
		}
		if len(rmdses) != 1 {
			return false, MDServerError{
				Err: fmt.Errorf("Expected 1 MD block got %d", len(rmdses)),
			}
		}
		head = rmdses[0]
		recordBranchID = true
	}

	// Consistency checks
	if head != nil {
		headID, err := s.crypto.MakeMdID(head.MD)
		if err != nil {
			return false, MDServerError{err}
		}

		err = head.MD.CheckValidSuccessorForServer(headID, rmds.MD)
		if err != nil {
			return false, err
		}
	}

	id, err := s.putMDLocked(rmds)
	if err != nil {
		return false, MDServerError{err}
	}

	err = s.putExtraMetadataLocked(rmds, extra)
	if err != nil {
		return false, MDServerError{err}
	}

	j, err := s.getOrCreateBranchJournalLocked(bid)
	if err != nil {
		return false, err
	}

	err = j.append(rmds.MD.RevisionNumber(), mdIDJournalEntry{ID: id})
	if err != nil {
		return false, MDServerError{err}
	}

	return recordBranchID, nil
}

func (s *mdServerTlfStorage) getKeyBundles(
	wkbID TLFWriterKeyBundleID, rkbID TLFReaderKeyBundleID) (
	*TLFWriterKeyBundleV3, *TLFReaderKeyBundleV3, error) {
	s.lock.Lock()
	defer s.lock.Unlock()
	return s.getKeyBundlesReadLocked(wkbID, rkbID)
}

func (s *mdServerTlfStorage) shutdown() {
	s.lock.Lock()
	defer s.lock.Unlock()
	s.branchJournals = nil
}
