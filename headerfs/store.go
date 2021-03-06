package headerfs

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/roasbeef/btcd/blockchain"
	"github.com/roasbeef/btcd/chaincfg"
	"github.com/roasbeef/btcd/chaincfg/chainhash"
	"github.com/roasbeef/btcd/wire"
	"github.com/roasbeef/btcutil/gcs/builder"
	"github.com/roasbeef/btcwallet/waddrmgr"
	"github.com/roasbeef/btcwallet/walletdb"
)

// headerBufPool is a pool of bytes.Buffer that will be re-used by the various
// headerStore implementations to batch their header writes to disk. By
// utilizing this variable we can minimize the total number of allocations when
// writing headers to disk.
var headerBufPool = sync.Pool{
	New: func() interface{} { return new(bytes.Buffer) },
}

// headerStore combines a on-disk set of headers within a flat file in addition
// to a databse which indexes tghat flat file. Together, these two abstractions
// can be used in order to build an indexed header store for any type of
// "header" as it deals only with raw bytes, and leaves it to a higher layer to
// interpret those raw bytes accordingly.
//
// TODO(roasbeef): quickcheck coverage
type headerStore struct {
	sync.RWMutex

	filePath string

	file *os.File

	*headerIndex
}

// newHeaderStore creates a new headerStore given an already open database, a
// target file path for the flat-file and a particular header type. The target
// file will be created as necessary.
func newHeaderStore(db walletdb.DB, filePath string,
	hType HeaderType) (*headerStore, error) {

	var flatFileName string
	switch hType {
	case Block:
		flatFileName = "block_headers.bin"
	case RegularFilter:
		flatFileName = "reg_filter_headers.bin"
	case ExtendedFilter:
		flatFileName = "ext_filter_headers.bin"
	default:
		return nil, fmt.Errorf("unrecognized filter type: %v", hType)
	}

	flatFileName = filepath.Join(filePath, flatFileName)

	// We'll open the file, creating it if necessary and ensuring that all
	// writes are actually appends to the end of the file.
	fileFlags := os.O_RDWR | os.O_APPEND | os.O_CREATE
	headerFile, err := os.OpenFile(flatFileName, fileFlags, 0755)
	if err != nil {
		return nil, err
	}

	// With the file open, we'll then create the header index so we can
	// have random access into the flat files.
	index, err := newHeaderIndex(db, hType)
	if err != nil {
		return nil, err
	}

	return &headerStore{
		filePath:    filePath,
		file:        headerFile,
		headerIndex: index,
	}, nil
}

// BlockHeaderStore is an implementation of a fully fledged database for
// Bitcoin block headers. The BlockHeaderStore combines a flat file to store
// the block headers with a database instance for managing the index into the
// set of flat files.
type BlockHeaderStore struct {
	*headerStore
}

// New creates a new instance of the BlockHeaderStore based on a target file
// path, an open database instance, and finally a set of parameters for the
// target chain. These parameters are required as if this is the initial start
// up of the BlockHeaderStore, then the initial genesis header will need to be
// inserted.
func NewBlockHeaderStore(filePath string, db walletdb.DB,
	netParams *chaincfg.Params) (*BlockHeaderStore, error) {

	hStore, err := newHeaderStore(db, filePath, Block)
	if err != nil {
		return nil, err
	}

	// With the header store created, we'll fetch the file size to see if
	// we need to initialize it with the first header or not.
	fileInfo, err := hStore.file.Stat()
	if err != nil {
		return nil, err
	}

	bhs := &BlockHeaderStore{
		headerStore: hStore,
	}

	// If the size of the file is zero, then this means that we haven't yet
	// written the initial genesis header to disk, so we'll do so now.
	if fileInfo.Size() == 0 {
		genesisHeader := BlockHeader{
			BlockHeader: &netParams.GenesisBlock.Header,
			Height:      0,
		}
		if err := bhs.WriteHeaders(genesisHeader); err != nil {
			return nil, err
		}

		return bhs, nil
	}

	// As a final initialization step (if this isn't the first time), we'll
	// ensure that the header tip within the flat files, is in sync with
	// out database index.
	tipHash, tipHeight, err := bhs.chainTip()
	if err != nil {
		return nil, err
	}

	// First, we'll compute the size of the current file so we can
	// calculate the latest header written to disk.
	fileHeight := (fileInfo.Size() / 80) - 1

	// Using the file's current height, fetch the latest on-disk header.
	latestFileHeader, err := bhs.readHeader(fileHeight)
	if err != nil {
		return nil, err
	}

	// If the index's tip hash, and the file on-disk match, then we're
	// doing here.
	latestBlockHash := latestFileHeader.BlockHash()
	if tipHash.IsEqual(&latestBlockHash) {
		return bhs, nil
	}

	// TODO(roasbeef): below assumes index can never get ahead?
	//  * we always update files _then_ indexes
	//  * need to dual pointer walk back for max safety

	// Otherwise, we'll need to truncate the file until it matches the
	// current index tip.
	for fileHeight > int64(tipHeight) {
		if bhs.singleTruncate(); err != nil {
			return nil, err
		}

		fileHeight--
	}

	return bhs, nil
}

// FetchHeader attempts to retrieve a block header determined by the passed
// block height.
func (h *BlockHeaderStore) FetchHeader(hash *chainhash.Hash) (*wire.BlockHeader, uint32, error) {
	// First, we'll query the index to obtain the block height of the
	// passed block hash.
	height, err := h.heightFromHash(hash)
	if err != nil {
		return nil, 0, err
	}

	// With the height known, we can now read the header from disk.
	header, err := h.readHeader(int64(height))
	if err != nil {
		return nil, 0, err
	}

	return header, height, nil
}

// FetchHeaderByHeight attempts to retrieve a target block header based on a
// block height.
func (h *BlockHeaderStore) FetchHeaderByHeight(height uint32) (*wire.BlockHeader, error) {
	// For this query, we don't need to consult the index, and can instead
	// just seek into the flat file based on the target height and return
	// the full header.
	return h.readHeader(int64(height))
}

// HeightFromHash returns the height of a particualr block header given its
// hash.
func (h *BlockHeaderStore) HeightFromHash(hash *chainhash.Hash) (uint32, error) {
	return h.heightFromHash(hash)
}

// RollbackLastBlock rollsback both the index, and on-disk header file by a
// _single_ header. This method is meant to be used in the case of re-org which
// disconnects the latest block header from the end of the main chain. The
// information about the new header tip after truncation is returned.
func (h *BlockHeaderStore) RollbackLastBlock() (*waddrmgr.BlockStamp, error) {
	// First, we'll obtain the latest height that the index knows of.
	_, chainTipHeight, err := h.chainTip()
	if err != nil {
		return nil, err
	}

	// With this height obtained, we'll use it to read the latest header
	// from disk, so we can populate our return value which requires the
	// prev header hash.
	bestHeader, err := h.readHeader(int64(chainTipHeight))
	if err != nil {
		return nil, err
	}
	prevHeaderHash := bestHeader.PrevBlock

	// Now that we have the information we need to return from this
	// function, we can now truncate the header file, and then use the hash
	// of the prevHeader to set the proper index chain tip.
	if err := h.singleTruncate(); err != nil {
		return nil, err
	}
	if err := h.truncateIndex(&prevHeaderHash, true); err != nil {
		return nil, err
	}

	return &waddrmgr.BlockStamp{
		Height: int32(chainTipHeight) - 1,
		Hash:   prevHeaderHash,
	}, nil
}

// BlockHeader is a Bitcoin block header that also has its height included.
type BlockHeader struct {
	*wire.BlockHeader

	// Height is the height of this block header within the current main
	// chain.
	Height uint32
}

// toIndexEntry converts the BlockHeader into a matching headerEntry. This
// method is used when a header is to be written to disk.
func (b *BlockHeader) toIndexEntry() headerEntry {
	return headerEntry{
		hash:   b.BlockHash(),
		height: b.Height,
	}
}

// WriteHeaders writes a set of headers to disk and updates the index in a
// single atomic transaction.
func (h *BlockHeaderStore) WriteHeaders(hdrs ...BlockHeader) error {
	// First, we'll grab a buffer from the write buffer pool so we an
	// reduce our total number of allocations, and also write the headers
	// in a single swoop.
	headerBuf := headerBufPool.Get().(*bytes.Buffer)
	headerBuf.Reset()
	defer headerBufPool.Put(headerBuf)

	// Next, we'll write out all the passed headers in series into the
	// buffer we just extracted from the pool.
	for _, header := range hdrs {
		if err := header.Serialize(headerBuf); err != nil {
			return err
		}
	}

	// With all the headers written to the buffer, we'll now write out the
	// entire batch in a single write call.
	if err := h.appendRaw(headerBuf.Bytes()); err != nil {
		return err
	}

	// Once those are written, we'll then collate all the headers into
	// headerEntry instances so we can write them all into the index in a
	// single atomic batch.
	headerLocs := make([]headerEntry, len(hdrs))
	for i, header := range hdrs {
		headerLocs[i] = header.toIndexEntry()
	}

	return h.addHeaders(headerLocs)
}

// blockLocatorFromHash takes a given block hash and then creates a block
// locator using it as the root of the locator. We'll start by taking a single
// step backwards, then keep doubling the distance until genesis after we get
// 10 locators.
//
// TODO(roasbeef): make into single transaction.
func (h *BlockHeaderStore) blockLocatorFromHash(hash *chainhash.Hash) (blockchain.BlockLocator, error) {
	var locator blockchain.BlockLocator

	// Append the initial hash
	locator = append(locator, hash)

	// If hash isn't found in DB or this is the genesis block, return the
	// locator as is
	height, err := h.heightFromHash(hash)
	if height == 0 || err != nil {
		return locator, nil
	}

	decrement := uint32(1)
	for height > 0 && len(locator) < wire.MaxBlockLocatorsPerMsg {
		// Decrement by 1 for the first 10 blocks, then double the jump
		// until we get to the genesis hash
		if len(locator) > 10 {
			decrement *= 2
		}

		if decrement > height {
			height = 0
		} else {
			height -= decrement
		}

		blockHeader, err := h.FetchHeaderByHeight(height)
		if err != nil {
			return locator, err
		}
		headerHash := blockHeader.BlockHash()

		locator = append(locator, &headerHash)
	}

	return locator, nil
}

// LatestBlockLocator returns the latest block locator object based on the tip
// of the current main chain from the PoV of the database and flat files.
func (h *BlockHeaderStore) LatestBlockLocator() (blockchain.BlockLocator, error) {
	var locator blockchain.BlockLocator

	chainTipHash, _, err := h.chainTip()
	if err != nil {
		return locator, err
	}

	return h.blockLocatorFromHash(chainTipHash)
}

// BlockLocatorFromHash computes a block locator given a particular hash. The
// standard Bitcoin algorithm to compute block locators are employed.
func (h *BlockHeaderStore) BlockLocatorFromHash(hash *chainhash.Hash) (blockchain.BlockLocator, error) {
	return h.blockLocatorFromHash(hash)
}

// CheckConnectivity cycles through all of the block headers on disk, from last
// to first, and makes sure they all connect to each other. Additionally, at
// each block header, we also ensure that the index entry for that height and
// hash also match up properly.
func (h *BlockHeaderStore) CheckConnectivity() error {
	return walletdb.View(h.db, func(tx walletdb.ReadTx) error {
		// First, we'll fetch the root bucket, in order to use that to
		// fetch the bucket that houses the header index.
		rootBucket := tx.ReadBucket(indexBucket)

		// With the header bucket retrieved, we'll now fetch the chain
		// tip so we can start our backwards scan.
		tipHash := rootBucket.Get(bitcoinTip)
		tipHeightBytes := rootBucket.Get(tipHash)

		// With the height extracted, we'll now read the _last_ block
		// header within the file before we kick off our connectivity
		// loop.
		tipHeight := int64(binary.BigEndian.Uint32(tipHeightBytes))
		header, err := h.readHeader(tipHeight)
		if err != nil {
			return err
		}

		// We'll now cycle backwards, seeking backwards along the
		// header file to ensure each header connects properly and the
		// index entries are also accurate. To do this, we start from a
		// height of one before our current tip.
		var newHeader *wire.BlockHeader
		for height := int64(tipHeight) - 1; height > 0; height-- {
			// First, read the block header for this block height,
			// and also compute the block hash for it.
			newHeader, err = h.readHeader(height)
			if err != nil {
				return fmt.Errorf("Couldn't retrieve header %s:"+
					" %s", header.PrevBlock, err)
			}
			newHeaderHash := newHeader.BlockHash()

			// With the header retrieved, we'll now fetch the
			// height for this current header hash to ensure the
			// on-disk state and the index matches up properly.
			indexHeightBytes := rootBucket.Get(newHeaderHash[:])
			if indexHeightBytes == nil {
				return fmt.Errorf("index and on-disk file out of sync "+
					"at height: %v", height)
			}
			indexHeight := int64(binary.BigEndian.Uint32(indexHeightBytes))

			// With the index entry retrieved, we'll now assert
			// that the height matches up with our current height
			// in this backwards walk.
			if int64(indexHeight) != height {
				return fmt.Errorf("index height isn't monotonically " +
					"increasing")
			}

			// Finally, we'll assert that this new header is
			// actually the prev header of the target header from
			// the last loop. This ensures connectivity.
			if newHeader.BlockHash() != header.PrevBlock {
				return fmt.Errorf("Block %s doesn't match "+
					"block %s's PrevBlock (%s)",
					newHeader.BlockHash(),
					header.BlockHash(), header.PrevBlock)
			}

			// As all the checks have passed, we'll now reset our
			// header pointer to this current location, and
			// continue our backwards walk.
			header = newHeader
		}

		return nil
	})
}

// ChainTip returns the best known block header and height for the
// BlockHeaderStore.
func (h *BlockHeaderStore) ChainTip() (*wire.BlockHeader, uint32, error) {
	_, tipHeight, err := h.chainTip()
	if err != nil {
		return nil, 0, err
	}

	latestHeader, err := h.readHeader(int64(tipHeight))
	if err != nil {
		return nil, 0, err
	}

	return latestHeader, tipHeight, nil
}

// FilterHeaderStore is an implementation of a fully fledged database for any
// variant of filter headers.  The FilterHeaderStore combines a flat file to
// store the block headers with a database instance for managing the index into
// the set of flat files.
type FilterHeaderStore struct {
	*headerStore
}

// NewFilterHeaderStore returns a new instance of the FilterHeaderStore based
// on a target file path, filter type, and target net parameters. These
// parameters are required as if this is the initial start up of the
// FilterHeaderStore, then the initial genesis filter header will need to be
// inserted.
func NewFilterHeaderStore(filePath string, db walletdb.DB,
	filterType HeaderType, netParams *chaincfg.Params) (*FilterHeaderStore, error) {

	fStore, err := newHeaderStore(db, filePath, filterType)
	if err != nil {
		return nil, err
	}

	// With the header store created, we'll fetch the fiie size to see if
	// we need to initialize it with the first header or not.
	fileInfo, err := fStore.file.Stat()
	if err != nil {
		return nil, err
	}

	fhs := &FilterHeaderStore{
		fStore,
	}

	// TODO(roasbeef): also reconsile with block header state due to way
	// roll back works atm

	// If the size of the file is zero, then this means that we haven't yet
	// written the initial genesis header to disk, so we'll do so now.
	if fileInfo.Size() == 0 {

		var genesisFilterHash chainhash.Hash
		switch filterType {
		case RegularFilter:
			basicFilter, err := builder.BuildBasicFilter(
				netParams.GenesisBlock,
			)
			if err != nil {
				return nil, err
			}

			genesisFilterHash = builder.MakeHeaderForFilter(
				basicFilter,
				netParams.GenesisBlock.Header.PrevBlock,
			)

		case ExtendedFilter:
			extFilter, err := builder.BuildExtFilter(
				netParams.GenesisBlock,
			)
			if err != nil {
				return nil, err
			}

			genesisFilterHash = builder.MakeHeaderForFilter(
				extFilter,
				netParams.GenesisBlock.Header.PrevBlock,
			)
		}

		genesisHeader := FilterHeader{
			HeaderHash: *netParams.GenesisHash,
			FilterHash: genesisFilterHash,
			Height:     0,
		}
		if err := fhs.WriteHeaders(genesisHeader); err != nil {
			return nil, err
		}

		return fhs, nil
	}

	// As a final initialization step, we'll ensure that the header tip
	// within the flat files, is in sync with out database index.
	tipHash, tipHeight, err := fhs.chainTip()
	if err != nil {
		return nil, err
	}

	// First, we'll compute the size of the current file so we can
	// calculate the latest header written to disk.
	fileHeight := (fileInfo.Size() / 32) - 1

	// Using the file's current height, fetch the latest on-disk header.
	latestFileHeader, err := fhs.readHeader(fileHeight)
	if err != nil {
		return nil, err
	}

	// If the index's tip hash, and the file on-disk match, then we're
	// doing here.
	if tipHash.IsEqual(latestFileHeader) {
		return fhs, nil
	}

	// Otherwise, we'll need to truncate the file until it matches the
	// current index tip.
	for fileHeight > int64(tipHeight) {
		if fhs.singleTruncate(); err != nil {
			return nil, err
		}

		fileHeight--
	}

	// TODO(roasbeef): make above into func

	return fhs, nil
}

// FetchHeader returns the filter header that corresponds to the passed block
// height.
func (f *FilterHeaderStore) FetchHeader(hash *chainhash.Hash) (*chainhash.Hash, error) {
	height, err := f.heightFromHash(hash)
	if err != nil {
		return nil, err
	}

	return f.readHeader(int64(height))
}

// FetchHeaderByHeight returns the filter header for a particular block height.
func (f *FilterHeaderStore) FetchHeaderByHeight(height uint32) (*chainhash.Hash, error) {
	return f.readHeader(int64(height))
}

// FilterHeader represents a filter header (basic or extended). The filter
// header itself is coupled with the block height and hash of the filter's
// block.
type FilterHeader struct {
	// HeaderHash is the hash of the block header that this filter header
	// corresponds to.
	HeaderHash chainhash.Hash

	// FilterHash is the filter header itself.
	FilterHash chainhash.Hash

	// Height is the block height of the filter header in the main chain.
	Height uint32
}

// toIndexEntry converts the filter header into a index entry to be stored
// within the database.
func (f *FilterHeader) toIndexEntry() headerEntry {
	return headerEntry{
		hash:   f.HeaderHash,
		height: f.Height,
	}
}

// WriteHeaders writes a batch of filter headers to persistent storage. The
// headers themselves are appended to the flat file, and then the index updated
// to reflect the new entires.
func (f *FilterHeaderStore) WriteHeaders(hdrs ...FilterHeader) error {
	// If there are 0 headers to be written, return immediately. This
	// prevents the newTip assignment from panicking because of an index
	// of -1.
	if len(hdrs) == 0 {
		return nil
	}

	// First, we'll grab a buffer from the write buffer pool so we an
	// reduce our total number of allocations, and also write the headers
	// in a single swoop.
	headerBuf := headerBufPool.Get().(*bytes.Buffer)
	headerBuf.Reset()
	defer headerBufPool.Put(headerBuf)

	// Next, we'll write out all the passed headers in series into the
	// buffer we just extracted from the pool.
	for _, header := range hdrs {
		if _, err := headerBuf.Write(header.FilterHash[:]); err != nil {
			return err
		}
	}

	// With all the headers written to the buffer, we'll now write out the
	// entire batch in a single write call.
	if err := f.appendRaw(headerBuf.Bytes()); err != nil {
		return err
	}

	// As the block headers should already be written, we only need to
	// update the tip pointer for this particular header type.
	newTip := hdrs[len(hdrs)-1].toIndexEntry().hash
	return f.truncateIndex(&newTip, false)
}

// ChainTip returns the latest filter header and height known to the
// FilterHeaderStore.
func (f *FilterHeaderStore) ChainTip() (*chainhash.Hash, uint32, error) {
	_, tipHeight, err := f.chainTip()
	if err != nil {
		return nil, 0, err
	}

	latestHeader, err := f.readHeader(int64(tipHeight))
	if err != nil {
		return nil, 0, err
	}

	return latestHeader, tipHeight, nil
}

// RollbackLastBlock rollsback both the index, and on-disk header file by a
// _single_ filter header. This method is meant to be used in the case of
// re-org which disconnects the latest filter header from the end of the main
// chain. The information about the latest header tip after truncation is
// returnd.
func (f *FilterHeaderStore) RollbackLastBlock(newTip *chainhash.Hash) (*waddrmgr.BlockStamp, error) {
	// First, we'll obtain the latest height that the index knows of.
	_, chainTipHeight, err := f.chainTip()
	if err != nil {
		return nil, err
	}

	// With this height obtained, we'll use it to read what will be the new
	// chain tip from disk.
	newHeightTip := chainTipHeight - 1
	newHeaderTip, err := f.readHeader(int64(newHeightTip))
	if err != nil {
		return nil, err
	}

	// Now that we have the information we need to return from this
	// function, we can now truncate both the header file and the index.
	if err := f.singleTruncate(); err != nil {
		return nil, err
	}
	if err := f.truncateIndex(newTip, false); err != nil {
		return nil, err
	}

	// TODO(roasbeef): return chain hash also?
	return &waddrmgr.BlockStamp{
		Height: int32(newHeightTip),
		Hash:   *newHeaderTip,
	}, nil
}
