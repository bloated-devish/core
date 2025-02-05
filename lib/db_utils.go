package lib

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"encoding/gob"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"math/big"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/btcsuite/btcd/btcec"
	"github.com/davecgh/go-spew/spew"
	"github.com/dgraph-io/badger/v3"
	"github.com/golang/glog"
	"github.com/pkg/errors"
)

// This file contains all of the functions that interact with the database.

const (
	// badgerDbFolder is the subfolder in the config dir where we
	// store the badgerdb database by default.
	badgerDbFolder = "badgerdb"
)

var (
	// The key prefixes for the key-value database. To store a particular
	// type of data, we create a key prefix and store all those types of
	// data with a key prefixed by that key prefix.
	// Bitcoin does a similar thing that you can see at this link:
	// https://bitcoin.stackexchange.com/questions/28168/what-are-the-keys-used-in-the-blockchain-leveldb-ie-what-are-the-keyvalue-pair

	// The prefix for the block index:
	// Key format: <hash BlockHash>
	// Value format: serialized MsgBitCloutBlock
	_PrefixBlockHashToBlock = []byte{0}

	// The prefix for the node index that we use to reconstruct the block tree.
	// Storing the height in big-endian byte order allows us to read in all the
	// blocks in height-sorted order from the db and construct the block tree by connecting
	// nodes to their parents as we go.
	//
	// Key format: <height uint32 (big-endian), hash BlockHash>
	// Value format: serialized BlockNode
	_PrefixHeightHashToNodeInfo        = []byte{1}
	_PrefixBitcoinHeightHashToNodeInfo = []byte{2}

	// We store the hash of the node that is the current tip of the main chain.
	// This key is used to look it up.
	// Value format: BlockHash
	_KeyBestBitCloutBlockHash = []byte{3}

	_KeyBestBitcoinHeaderHash = []byte{4}

	// Utxo table.
	// <txid BlockHash, output_index uint64> -> UtxoEntry
	_PrefixUtxoKeyToUtxoEntry = []byte{5}
	// <prefix, pubKey [33]byte, utxoKey< txid BlockHash, index uint32 >> -> <>
	_PrefixPubKeyUtxoKey = []byte{7}
	// The number of utxo entries in the database.
	_KeyUtxoNumEntries = []byte{8}
	// Utxo operations table.
	// This table contains, for each blockhash on the main chain, the UtxoOperations
	// that were applied by this block. To roll back the block, one must loop through
	// the UtxoOperations for a particular block backwards and invert them.
	//
	// < hash *BlockHash > -> < serialized []UtxoOperation using gob encoding >
	_PrefixBlockHashToUtxoOperations = []byte{9}

	// The below are mappings related to the validation of BitcoinExchange transactions.
	//
	// The number of nanos that has been purchased thus far.
	_KeyNanosPurchased = []byte{10}
	// How much Bitcoin is work in USD cents.
	_KeyUSDCentsPerBitcoinExchangeRate = []byte{27}
	// <key> -> <GlobalParamsEntry gob serialized>
	_KeyGlobalParams = []byte{40}

	// The prefix for the Bitcoin TxID map. If a key is set for a TxID that means this
	// particular TxID has been processed as part of a BitcoinExchange transaction. If
	// no key is set for a TxID that means it has not been processed (and thus it can be
	// used to create new nanos).
	// <BitcoinTxID BlockHash> -> <nothing>
	_PrefixBitcoinBurnTxIDs = []byte{11}

	// Messages are indexed by the public key of their senders and receivers. If
	// a message sends from pkFrom to pkTo then there will be two separate entries,
	// one for pkFrom and one for pkTo. The exact format is as follows:
	// <public key (33 bytes) || uint64 big-endian> -> < SenderPublicKey || RecipientPublicKey || EncryptedText >
	_PrefixPublicKeyTimestampToPrivateMessage = []byte{12}

	// Tracks the tip of the transaction index. This is used to determine
	// which blocks need to be processed in order to update the index.
	_KeyTransactionIndexTip = []byte{14}
	// <prefix, transactionID BlockHash> -> <TransactionMetadata struct>
	_PrefixTransactionIDToMetadata = []byte{15}
	// <prefix, publicKey []byte, index uint32> -> <txid BlockHash>
	_PrefixPublicKeyIndexToTransactionIDs = []byte{16}
	// <prefx, publicKey []byte> -> <index uint32>
	_PrefixPublicKeyToNextIndex = []byte{42}

	// Main post index.
	// <prefix, PostHash BlockHash> -> PostEntry
	_PrefixPostHashToPostEntry = []byte{17}

	// Post sorts
	// <prefix, publicKey [33]byte, PostHash> -> <>
	_PrefixPosterPublicKeyPostHash = []byte{18}

	// <prefix, tstampNanos uint64, PostHash> -> <>
	_PrefixTstampNanosPostHash = []byte{19}
	// <prefix, creatorbps uint64, PostHash> -> <>
	_PrefixCreatorBpsPostHash = []byte{20}
	// <prefix, multiplebps uint64, PostHash> -> <>
	_PrefixMultipleBpsPostHash = []byte{21}

	// Comments are just posts that have their ParentStakeID set, and
	// so we have a separate index that allows us to return all the
	// comments for a given StakeID
	// <prefix, parent stakeID [33]byte, tstampnanos uint64, post hash> -> <>
	_PrefixCommentParentStakeIDToPostHash = []byte{22}

	// Main profile index
	// <prefix, PKID [33]byte> -> ProfileEntry
	_PrefixPKIDToProfileEntry = []byte{23}

	// Profile sorts
	// For username, we set the PKID as a value since the username is not fixed width.
	// We always lowercase usernames when using them as map keys in order to make
	// all uniqueness checks case-insensitive
	// <prefix, username> -> <PKID>
	_PrefixProfileUsernameToPKID = []byte{25}
	// This allows us to sort the profiles by the value of their coin (since
	// the amount of BitClout locked in a profile is proportional to coin price).
	_PrefixCreatorBitCloutLockedNanosCreatorPKID = []byte{32}

	// The StakeID is a post hash for posts and a public key for users.
	// <StakeIDType | AmountNanos uint64 | StakeID [var]byte> -> <>
	_PrefixStakeIDTypeAmountStakeIDIndex = []byte{26}

	// Prefixes for follows:
	// <prefix, follower PKID [33]byte, followed PKID [33]byte> -> <>
	// <prefix, followed PKID [33]byte, follower PKID [33]byte> -> <>
	_PrefixFollowerPKIDToFollowedPKID = []byte{28}
	_PrefixFollowedPKIDToFollowerPKID = []byte{29}

	// Prefixes for likes:
	// <prefix, user pub key [33]byte, liked post hash [32]byte> -> <>
	// <prefix, post hash [32]byte, user pub key [33]byte> -> <>
	_PrefixLikerPubKeyToLikedPostHash = []byte{30}
	_PrefixLikedPostHashToLikerPubKey = []byte{31}

	// Prefixes for creator coin fields:
	// <prefix, HODLer PKID [33]byte, creator PKID [33]byte> -> <BalanceEntry>
	// <prefix, creator PKID [33]byte, HODLer PKID [33]byte> -> <BalanceEntry>
	_PrefixHODLerPKIDCreatorPKIDToBalanceEntry = []byte{33}
	_PrefixCreatorPKIDHODLerPKIDToBalanceEntry = []byte{34}

	_PrefixPosterPublicKeyTimestampPostHash = []byte{35}

	// If no mapping exists for a particular public key, then the PKID is simply
	// the public key itself.
	// <[33]byte> -> <PKID [33]byte>
	_PrefixPublicKeyToPKID = []byte{36}
	// <PKID [33]byte> -> <PublicKey [33]byte>
	_PrefixPKIDToPublicKey = []byte{37}

	// Prefix for storing mempool transactions in badger. These stored transactions are
	// used to restore the state of a node after it is shutdown.
	// <prefix, tx hash BlockHash> -> <*MsgBitCloutTxn>
	_PrefixMempoolTxnHashToMsgBitCloutTxn = []byte{38}

	// Prefixes for Reclouts:
	// <prefix, user pub key [39]byte, reclouted post hash [39]byte> -> RecloutEntry
	_PrefixReclouterPubKeyRecloutedPostHashToRecloutPostHash = []byte{39}
	// TODO: This process is a bit error-prone. We should come up with a test or
	// something to at least catch cases where people have two prefixes with the
	// same ID.

	// Prefixes for diamonds:
	//  <prefix, DiamondReceiverPKID [33]byte, DiamondSenderPKID [33]byte, posthash> -> <gob-encoded DiamondEntry>
	//  <prefix, DiamondSenderPKID [33]byte, DiamondReceiverPKID [33]byte, posthash> -> <gob-encoded DiamondEntry>
	_PrefixDiamondReceiverPKIDDiamondSenderPKIDPostHash = []byte{41}
	_PrefixDiamondSenderPKIDDiamondReciverPKIDPostHash  = []byte{43}

	// Public keys that have been restricted from signing blocks.
	// <prefix, ForbiddenPublicKey [33]byte> -> <>
	_PrefixForbiddenBlockSignaturePubKeys = []byte{44}

	// NEXT_TAG: 45
)

// A PKID is an ID associated with a public key. In the DB, various fields are
// indexed using the PKID rather than the user's public key directly in order to
// create one layer of indirection between the public key and the user's data. This
// makes it easy for the user to transfer certain data to a new public key.
type PKID [33]byte

func PublicKeyToPKID(publicKey []byte) *PKID {
	if len(publicKey) == 0 {
		return nil
	}
	pkid := &PKID{}
	copy(pkid[:], publicKey)
	return pkid
}

func PKIDToPublicKey(pkid *PKID) []byte {
	if pkid == nil {
		return nil
	}
	return pkid[:]
}

func DBGetPKIDEntryForPublicKeyWithTxn(txn *badger.Txn, publicKey []byte) *PKIDEntry {
	if len(publicKey) == 0 {
		return nil
	}

	prefix := append([]byte{}, _PrefixPublicKeyToPKID...)
	pkidItem, err := txn.Get(append(prefix, publicKey...))

	if err != nil {
		// If we don't have a mapping from public key to PKID in the db,
		// then we use the public key itself as the PKID. Doing this makes
		// it so that the PKID is generally the *first* public key that the
		// user ever associated with a particular piece of data.
		return &PKIDEntry{
			PKID:      PublicKeyToPKID(publicKey),
			PublicKey: publicKey,
		}
	}

	// If we get here then it means we actually had a PKID in the DB.
	// So return that pkid.
	pkidEntryObj := &PKIDEntry{}
	err = pkidItem.Value(func(valBytes []byte) error {
		return gob.NewDecoder(bytes.NewReader(valBytes)).Decode(pkidEntryObj)
	})
	if err != nil {
		glog.Errorf("DBGetPKIDEntryForPublicKeyWithTxn: Problem reading "+
			"PKIDEntry for public key %s",
			PkToStringMainnet(publicKey))
		return nil
	}
	return pkidEntryObj
}

func DBGetPKIDEntryForPublicKey(db *badger.DB, publicKey []byte) *PKIDEntry {
	var pkid *PKIDEntry
	db.View(func(txn *badger.Txn) error {
		pkid = DBGetPKIDEntryForPublicKeyWithTxn(txn, publicKey)
		return nil
	})
	return pkid
}

func DBGetPublicKeyForPKIDWithTxn(txn *badger.Txn, pkidd *PKID) []byte {
	prefix := append([]byte{}, _PrefixPKIDToPublicKey...)
	pkidItem, err := txn.Get(append(prefix, pkidd[:]...))

	if err != nil {
		// If we don't have a mapping in the db then return the pkid itself
		// as the public key.
		return pkidd[:]
	}

	// If we get here then it means we actually had a public key mapping in the DB.
	// So return that public key.
	pkRet, err := pkidItem.ValueCopy(nil)
	if err != nil {
		// If we had a problem reading the mapping then log an error and return nil.
		glog.Errorf("DBGetPublicKeyForPKIDWithTxn: Problem reading "+
			"public key for pkid %s",
			PkToStringMainnet(pkidd[:]))
		return nil
	}

	return pkRet
}

func DBGetPublicKeyForPKID(db *badger.DB, pkidd *PKID) []byte {
	var publicKey []byte
	db.View(func(txn *badger.Txn) error {
		publicKey = DBGetPublicKeyForPKIDWithTxn(txn, pkidd)
		return nil
	})
	return publicKey
}

func DBPutPKIDMappingsWithTxn(
	txn *badger.Txn, publicKey []byte, pkidEntry *PKIDEntry, params *BitCloutParams) error {

	// Set the main pub key -> pkid mapping.
	{
		pkidDataBuf := bytes.NewBuffer([]byte{})
		gob.NewEncoder(pkidDataBuf).Encode(pkidEntry)

		prefix := append([]byte{}, _PrefixPublicKeyToPKID...)
		pubKeyToPkidKey := append(prefix, publicKey...)
		if err := txn.Set(pubKeyToPkidKey, pkidDataBuf.Bytes()); err != nil {

			return errors.Wrapf(err, "DBPutPKIDMappingsWithTxn: Problem "+
				"adding mapping for pkid: %v public key: %v",
				PkToString(pkidEntry.PKID[:], params), PkToString(publicKey, params))
		}
	}

	// Set the reverse mapping: pkid -> pub key
	{
		prefix := append([]byte{}, _PrefixPKIDToPublicKey...)
		pkidToPubKey := append(prefix, pkidEntry.PKID[:]...)
		if err := txn.Set(pkidToPubKey, publicKey); err != nil {

			return errors.Wrapf(err, "DBPutPKIDMappingsWithTxn: Problem "+
				"adding mapping for pkid: %v public key: %v",
				PkToString(pkidEntry.PKID[:], params), PkToString(publicKey, params))
		}
	}

	return nil
}

func DBDeletePKIDMappingsWithTxn(
	txn *badger.Txn, publicKey []byte, params *BitCloutParams) error {

	// Look up the pkid for the public key.
	pkidEntry := DBGetPKIDEntryForPublicKeyWithTxn(txn, publicKey)

	{
		prefix := append([]byte{}, _PrefixPublicKeyToPKID...)
		pubKeyToPkidKey := append(prefix, publicKey...)
		if err := txn.Delete(pubKeyToPkidKey); err != nil {

			return errors.Wrapf(err, "DBDeletePKIDMappingsWithTxn: Problem "+
				"deleting mapping for public key: %v",
				PkToString(publicKey, params))
		}
	}

	{
		prefix := append([]byte{}, _PrefixPKIDToPublicKey...)
		pubKeyToPkidKey := append(prefix, pkidEntry.PKID[:]...)
		if err := txn.Delete(pubKeyToPkidKey); err != nil {

			return errors.Wrapf(err, "DBDeletePKIDMappingsWithTxn: Problem "+
				"deleting mapping for pkid: %v",
				PkToString(pkidEntry.PKID[:], params))
		}
	}

	return nil
}

func EnumerateKeysForPrefix(db *badger.DB, dbPrefix []byte) (_keysFound [][]byte, _valsFound [][]byte) {
	return _enumerateKeysForPrefix(db, dbPrefix)
}

// A helper function to enumerate all of the values for a particular prefix.
func _enumerateKeysForPrefix(db *badger.DB, dbPrefix []byte) (_keysFound [][]byte, _valsFound [][]byte) {
	keysFound := [][]byte{}
	valsFound := [][]byte{}

	dbErr := db.View(func(txn *badger.Txn) error {
		var err error
		keysFound, valsFound, err = _enumerateKeysForPrefixWithTxn(txn, dbPrefix)
		if err != nil {
			return err
		}
		return nil
	})
	if dbErr != nil {
		glog.Errorf("_enumerateKeysForPrefix: Problem fetching keys and vlaues from db: %v", dbErr)
		return nil, nil
	}

	return keysFound, valsFound
}

func _enumerateKeysForPrefixWithTxn(dbTxn *badger.Txn, dbPrefix []byte) (_keysFound [][]byte, _valsFound [][]byte, _err error) {
	keysFound := [][]byte{}
	valsFound := [][]byte{}

	opts := badger.DefaultIteratorOptions
	nodeIterator := dbTxn.NewIterator(opts)
	defer nodeIterator.Close()
	prefix := dbPrefix
	for nodeIterator.Seek(prefix); nodeIterator.ValidForPrefix(prefix); nodeIterator.Next() {
		key := nodeIterator.Item().Key()
		keyCopy := make([]byte, len(key))
		copy(keyCopy[:], key[:])

		valCopy, err := nodeIterator.Item().ValueCopy(nil)
		if err != nil {
			return nil, nil, err
		}
		keysFound = append(keysFound, keyCopy)
		valsFound = append(valsFound, valCopy)
	}
	return keysFound, valsFound, nil
}

// A helper function to enumerate a limited number of the values for a particular prefix.
func _enumerateLimitedKeysReversedForPrefix(db *badger.DB, dbPrefix []byte, limit uint64) (_keysFound [][]byte, _valsFound [][]byte) {
	keysFound := [][]byte{}
	valsFound := [][]byte{}

	dbErr := db.View(func(txn *badger.Txn) error {
		var err error
		keysFound, valsFound, err = _enumerateLimitedKeysReversedForPrefixWithTxn(txn, dbPrefix, limit)
		return err
	})
	if dbErr != nil {
		glog.Errorf("_enumerateKeysForPrefix: Problem fetching keys and vlaues from db: %v", dbErr)
		return nil, nil
	}

	return keysFound, valsFound
}

func _enumerateLimitedKeysReversedForPrefixWithTxn(dbTxn *badger.Txn, dbPrefix []byte, limit uint64) (_keysFound [][]byte, _valsFound [][]byte, _err error) {
	keysFound := [][]byte{}
	valsFound := [][]byte{}

	opts := badger.DefaultIteratorOptions

	// Go in reverse order
	opts.Reverse = true

	nodeIterator := dbTxn.NewIterator(opts)
	defer nodeIterator.Close()
	prefix := dbPrefix

	counter := uint64(0)
	for nodeIterator.Seek(append(prefix, 0xff)); nodeIterator.ValidForPrefix(prefix); nodeIterator.Next() {
		if counter == limit {
			break
		}
		counter++

		key := nodeIterator.Item().Key()
		keyCopy := make([]byte, len(key))
		copy(keyCopy[:], key[:])

		valCopy, err := nodeIterator.Item().ValueCopy(nil)
		if err != nil {
			return nil, nil, err
		}
		keysFound = append(keysFound, keyCopy)
		valsFound = append(valsFound, valCopy)
	}
	return keysFound, valsFound, nil
}

// -------------------------------------------------------------------------------------
// PrivateMessage mapping functions
// <public key (33 bytes) || uint64 big-endian> ->
// 		< SenderPublicKey || RecipientPublicKey || EncryptedText >
// -------------------------------------------------------------------------------------

func _dbKeyForMessageEntry(publicKey []byte, tstampNanos uint64) []byte {
	// Make a copy to avoid multiple calls to this function re-using the same slice.
	prefixCopy := append([]byte{}, _PrefixPublicKeyTimestampToPrivateMessage...)
	key := append(prefixCopy, publicKey...)
	key = append(key, EncodeUint64(tstampNanos)...)
	return key
}

func _dbSeekPrefixForMessagePublicKey(publicKey []byte) []byte {
	// Make a copy to avoid multiple calls to this function re-using the same slice.
	prefixCopy := append([]byte{}, _PrefixPublicKeyTimestampToPrivateMessage...)
	return append(prefixCopy, publicKey...)
}

// Note that this adds a mapping for the sender *and* the recipient.
func DbPutMessageEntryWithTxn(
	txn *badger.Txn, messageEntry *MessageEntry) error {

	if len(messageEntry.SenderPublicKey) != btcec.PubKeyBytesLenCompressed {
		return fmt.Errorf("DbPutPrivateMessageWithTxn: Sender public key "+
			"length %d != %d", len(messageEntry.SenderPublicKey), btcec.PubKeyBytesLenCompressed)
	}
	if len(messageEntry.RecipientPublicKey) != btcec.PubKeyBytesLenCompressed {
		return fmt.Errorf("DbPutPrivateMessageWithTxn: Recipient public key "+
			"length %d != %d", len(messageEntry.RecipientPublicKey), btcec.PubKeyBytesLenCompressed)
	}
	messageData := &MessageEntry{
		SenderPublicKey:    messageEntry.SenderPublicKey,
		RecipientPublicKey: messageEntry.RecipientPublicKey,
		EncryptedText:      messageEntry.EncryptedText,
		TstampNanos:        messageEntry.TstampNanos,
	}

	messageDataBuf := bytes.NewBuffer([]byte{})
	gob.NewEncoder(messageDataBuf).Encode(messageData)

	if err := txn.Set(_dbKeyForMessageEntry(
		messageEntry.SenderPublicKey, messageEntry.TstampNanos), messageDataBuf.Bytes()); err != nil {

		return errors.Wrapf(err, "DbPutMessageEntryWithTxn: Problem adding mapping for sender: ")
	}
	if err := txn.Set(_dbKeyForMessageEntry(
		messageEntry.RecipientPublicKey, messageEntry.TstampNanos), messageDataBuf.Bytes()); err != nil {

		return errors.Wrapf(err, "DbPutMessageEntryWithTxn: Problem adding mapping for recipient: ")
	}

	return nil
}

func DbPutMessageEntry(handle *badger.DB, messageEntry *MessageEntry) error {

	return handle.Update(func(txn *badger.Txn) error {
		return DbPutMessageEntryWithTxn(txn, messageEntry)
	})
}

func DbGetMessageEntryWithTxn(
	txn *badger.Txn, publicKey []byte, tstampNanos uint64) *MessageEntry {

	key := _dbKeyForMessageEntry(publicKey, tstampNanos)
	privateMessageObj := &MessageEntry{}
	privateMessageItem, err := txn.Get(key)
	if err != nil {
		return nil
	}
	err = privateMessageItem.Value(func(valBytes []byte) error {
		return gob.NewDecoder(bytes.NewReader(valBytes)).Decode(privateMessageObj)
	})
	if err != nil {
		glog.Errorf("DbGetMessageEntryWithTxn: Problem reading "+
			"MessageEntry for public key %s with tstampnanos %d",
			PkToStringMainnet(publicKey), tstampNanos)
		return nil
	}
	return privateMessageObj
}

func DbGetMessageEntry(db *badger.DB, publicKey []byte, tstampNanos uint64) *MessageEntry {
	var ret *MessageEntry
	db.View(func(txn *badger.Txn) error {
		ret = DbGetMessageEntryWithTxn(txn, publicKey, tstampNanos)
		return nil
	})
	return ret
}

// Note this deletes the message for the sender *and* receiver since a mapping
// should exist for each.
func DbDeleteMessageEntryMappingsWithTxn(
	txn *badger.Txn, publicKey []byte, tstampNanos uint64) error {

	// First pull up the mapping that exists for the public key passed in.
	// If one doesn't exist then there's nothing to do.
	existingMessage := DbGetMessageEntryWithTxn(txn, publicKey, tstampNanos)
	if existingMessage == nil {
		return nil
	}

	// When a message exists, delete the mapping for the sender and receiver.
	if err := txn.Delete(_dbKeyForMessageEntry(existingMessage.SenderPublicKey, tstampNanos)); err != nil {
		return errors.Wrapf(err, "DbDeleteMessageEntryMappingsWithTxn: Deleting "+
			"sender mapping for public key %s and tstamp %d failed",
			PkToStringMainnet(existingMessage.SenderPublicKey), tstampNanos)
	}
	if err := txn.Delete(_dbKeyForMessageEntry(existingMessage.RecipientPublicKey, tstampNanos)); err != nil {
		return errors.Wrapf(err, "DbDeleteMessageEntryMappingsWithTxn: Deleting "+
			"recipient mapping for public key %s and tstamp %d failed",
			PkToStringMainnet(existingMessage.RecipientPublicKey), tstampNanos)
	}

	return nil
}

func DbDeleteMessageEntryMappings(handle *badger.DB, publicKey []byte, tstampNanos uint64) error {
	return handle.Update(func(txn *badger.Txn) error {
		return DbDeleteMessageEntryMappingsWithTxn(txn, publicKey, tstampNanos)
	})
}

func DbGetMessageEntriesForPublicKey(handle *badger.DB, publicKey []byte) (
	_privateMessages []*MessageEntry, _err error) {

	// Setting the prefix to a tstamp of zero should return all the messages
	// for the public key in sorted order since 0 << the minimum timestamp in
	// the db.
	prefix := _dbSeekPrefixForMessagePublicKey(publicKey)

	// Goes backwards to get messages in time sorted order.
	// Limit the number of keys to speed up load times.
	_, valuesFound := _enumerateKeysForPrefix(handle, prefix)

	privateMessages := []*MessageEntry{}
	for _, valBytes := range valuesFound {
		privateMessageObj := &MessageEntry{}
		if err := gob.NewDecoder(bytes.NewReader(valBytes)).Decode(privateMessageObj); err != nil {
			return nil, errors.Wrapf(
				err, "DbGetMessageEntriesForPublicKey: Problem decoding value: ")
		}

		privateMessages = append(privateMessages, privateMessageObj)
	}

	return privateMessages, nil
}

func DbGetLimitedMessageEntriesForPublicKey(handle *badger.DB, publicKey []byte) (
	_privateMessages []*MessageEntry, _err error) {

	// Setting the prefix to a tstamp of zero should return all the messages
	// for the public key in sorted order since 0 << the minimum timestamp in
	// the db.
	prefix := _dbSeekPrefixForMessagePublicKey(publicKey)

	// Goes backwards to get messages in time sorted order.
	// Limit the number of keys to speed up load times.
	_, valuesFound := _enumerateLimitedKeysReversedForPrefix(handle, prefix, uint64(MessagesToFetchPerInboxCall))

	privateMessages := []*MessageEntry{}
	for _, valBytes := range valuesFound {
		privateMessageObj := &MessageEntry{}
		if err := gob.NewDecoder(bytes.NewReader(valBytes)).Decode(privateMessageObj); err != nil {
			return nil, errors.Wrapf(
				err, "DbGetMessageEntriesForPublicKey: Problem decoding value: ")
		}

		privateMessages = append(privateMessages, privateMessageObj)
	}

	return privateMessages, nil
}

// -------------------------------------------------------------------------------------
// Forbidden block signature public key functions
// <prefix, public key> -> <>
// -------------------------------------------------------------------------------------

func _dbKeyForForbiddenBlockSignaturePubKeys(publicKey []byte) []byte {
	// Make a copy to avoid multiple calls to this function re-using the same slice.
	prefixCopy := append([]byte{}, _PrefixForbiddenBlockSignaturePubKeys...)
	key := append(prefixCopy, publicKey...)
	return key
}

func DbPutForbiddenBlockSignaturePubKeyWithTxn(txn *badger.Txn, publicKey []byte) error {

	if len(publicKey) != btcec.PubKeyBytesLenCompressed {
		return fmt.Errorf("DbPutForbiddenBlockSignaturePubKeyWithTxn: Forbidden public key "+
			"length %d != %d", len(publicKey), btcec.PubKeyBytesLenCompressed)
	}

	if err := txn.Set(_dbKeyForForbiddenBlockSignaturePubKeys(publicKey), []byte{}); err != nil {
		return errors.Wrapf(err, "DbPutForbiddenBlockSignaturePubKeyWithTxn: Problem adding mapping for sender: ")
	}

	return nil
}

func DbPutForbiddenBlockSignaturePubKey(handle *badger.DB, publicKey []byte) error {

	return handle.Update(func(txn *badger.Txn) error {
		return DbPutForbiddenBlockSignaturePubKeyWithTxn(txn, publicKey)
	})
}

func DbGetForbiddenBlockSignaturePubKeyWithTxn(txn *badger.Txn, publicKey []byte) []byte {

	key := _dbKeyForForbiddenBlockSignaturePubKeys(publicKey)
	_, err := txn.Get(key)
	if err != nil {
		return nil
	}

	// Typically we return a DB entry here but we don't store anything for this mapping.
	// We use this function instead of one returning true / false for feature consistency.
	return []byte{}
}

func DbGetForbiddenBlockSignaturePubKey(db *badger.DB, publicKey []byte) []byte {
	var ret []byte
	db.View(func(txn *badger.Txn) error {
		ret = DbGetForbiddenBlockSignaturePubKeyWithTxn(txn, publicKey)
		return nil
	})
	return ret
}

func DbDeleteForbiddenBlockSignaturePubKeyWithTxn(txn *badger.Txn, publicKey []byte) error {

	existingEntry := DbGetForbiddenBlockSignaturePubKeyWithTxn(txn, publicKey)
	if existingEntry == nil {
		return nil
	}

	if err := txn.Delete(_dbKeyForForbiddenBlockSignaturePubKeys(publicKey)); err != nil {
		return errors.Wrapf(err, "DbDeleteForbiddenBlockSignaturePubKeyWithTxn: Deleting "+
			"sender mapping for public key %s failed", PkToStringMainnet(publicKey))
	}

	return nil
}

func DbDeleteForbiddenBlockSignaturePubKey(handle *badger.DB, publicKey []byte) error {
	return handle.Update(func(txn *badger.Txn) error {
		return DbDeleteForbiddenBlockSignaturePubKeyWithTxn(txn, publicKey)
	})
}

// -------------------------------------------------------------------------------------
// Likes mapping functions
// 		<prefix, user pub key [33]byte, liked post BlockHash> -> <>
// 		<prefix, liked post BlockHash, user pub key [33]byte> -> <>
// -------------------------------------------------------------------------------------

func _dbKeyForLikerPubKeyToLikedPostHashMapping(
	userPubKey []byte, likedPostHash BlockHash) []byte {
	// Make a copy to avoid multiple calls to this function re-using the same slice.
	prefixCopy := append([]byte{}, _PrefixLikerPubKeyToLikedPostHash...)
	key := append(prefixCopy, userPubKey...)
	key = append(key, likedPostHash[:]...)
	return key
}

func _dbKeyForLikedPostHashToLikerPubKeyMapping(
	likedPostHash BlockHash, userPubKey []byte) []byte {
	// Make a copy to avoid multiple calls to this function re-using the same slice.
	prefixCopy := append([]byte{}, _PrefixLikedPostHashToLikerPubKey...)
	key := append(prefixCopy, likedPostHash[:]...)
	key = append(key, userPubKey...)
	return key
}

func _dbSeekPrefixForPostHashesYouLike(yourPubKey []byte) []byte {
	// Make a copy to avoid multiple calls to this function re-using the same slice.
	prefixCopy := append([]byte{}, _PrefixLikerPubKeyToLikedPostHash...)
	return append(prefixCopy, yourPubKey...)
}

func _dbSeekPrefixForLikerPubKeysLikingAPostHash(likedPostHash BlockHash) []byte {
	// Make a copy to avoid multiple calls to this function re-using the same slice.
	prefixCopy := append([]byte{}, _PrefixLikedPostHashToLikerPubKey...)
	return append(prefixCopy, likedPostHash[:]...)
}

// Note that this adds a mapping for the user *and* the liked post.
func DbPutLikeMappingsWithTxn(
	txn *badger.Txn, userPubKey []byte, likedPostHash BlockHash) error {

	if len(userPubKey) != btcec.PubKeyBytesLenCompressed {
		return fmt.Errorf("DbPutLikeMappingsWithTxn: User public key "+
			"length %d != %d", len(userPubKey), btcec.PubKeyBytesLenCompressed)
	}

	if err := txn.Set(_dbKeyForLikerPubKeyToLikedPostHashMapping(
		userPubKey, likedPostHash), []byte{}); err != nil {

		return errors.Wrapf(
			err, "DbPutLikeMappingsWithTxn: Problem adding user to liked post mapping: ")
	}
	if err := txn.Set(_dbKeyForLikedPostHashToLikerPubKeyMapping(
		likedPostHash, userPubKey), []byte{}); err != nil {

		return errors.Wrapf(
			err, "DbPutLikeMappingsWithTxn: Problem adding liked post to user mapping: ")
	}

	return nil
}

func DbPutLikeMappings(
	handle *badger.DB, userPubKey []byte, likedPostHash BlockHash) error {

	return handle.Update(func(txn *badger.Txn) error {
		return DbPutLikeMappingsWithTxn(txn, userPubKey, likedPostHash)
	})
}

func DbGetLikerPubKeyToLikedPostHashMappingWithTxn(
	txn *badger.Txn, userPubKey []byte, likedPostHash BlockHash) []byte {

	key := _dbKeyForLikerPubKeyToLikedPostHashMapping(userPubKey, likedPostHash)
	_, err := txn.Get(key)
	if err != nil {
		return nil
	}

	// Typically we return a DB entry here but we don't store anything for like mappings.
	// We use this function instead of one returning true / false for feature consistency.
	return []byte{}
}

func DbGetLikerPubKeyToLikedPostHashMapping(
	db *badger.DB, userPubKey []byte, likedPostHash BlockHash) []byte {
	var ret []byte
	db.View(func(txn *badger.Txn) error {
		ret = DbGetLikerPubKeyToLikedPostHashMappingWithTxn(txn, userPubKey, likedPostHash)
		return nil
	})
	return ret
}

// Note this deletes the like for the user *and* the liked post since a mapping
// should exist for each.
func DbDeleteLikeMappingsWithTxn(
	txn *badger.Txn, userPubKey []byte, likedPostHash BlockHash) error {

	// First check that a mapping exists. If one doesn't exist then there's nothing to do.
	existingMapping := DbGetLikerPubKeyToLikedPostHashMappingWithTxn(
		txn, userPubKey, likedPostHash)
	if existingMapping == nil {
		return nil
	}

	// When a message exists, delete the mapping for the sender and receiver.
	if err := txn.Delete(
		_dbKeyForLikerPubKeyToLikedPostHashMapping(userPubKey, likedPostHash)); err != nil {
		return errors.Wrapf(err, "DbDeleteLikeMappingsWithTxn: Deleting "+
			"userPubKey %s and likedPostHash %s failed",
			PkToStringBoth(userPubKey), likedPostHash)
	}
	if err := txn.Delete(
		_dbKeyForLikedPostHashToLikerPubKeyMapping(likedPostHash, userPubKey)); err != nil {
		return errors.Wrapf(err, "DbDeleteLikeMappingsWithTxn: Deleting "+
			"likedPostHash %s and userPubKey %s failed",
			PkToStringBoth(likedPostHash[:]), PkToStringBoth(userPubKey))
	}

	return nil
}

func DbDeleteLikeMappings(
	handle *badger.DB, userPubKey []byte, likedPostHash BlockHash) error {
	return handle.Update(func(txn *badger.Txn) error {
		return DbDeleteLikeMappingsWithTxn(txn, userPubKey, likedPostHash)
	})
}

func DbGetPostHashesYouLike(handle *badger.DB, yourPublicKey []byte) (
	_postHashes []*BlockHash, _err error) {

	prefix := _dbSeekPrefixForPostHashesYouLike(yourPublicKey)
	keysFound, _ := _enumerateKeysForPrefix(handle, prefix)

	postHashesYouLike := []*BlockHash{}
	for _, keyBytes := range keysFound {
		// We must slice off the first byte and userPubKey to get the likedPostHash.
		postHash := &BlockHash{}
		copy(postHash[:], keyBytes[1+btcec.PubKeyBytesLenCompressed:])
		postHashesYouLike = append(postHashesYouLike, postHash)
	}

	return postHashesYouLike, nil
}

func DbGetLikerPubKeysLikingAPostHash(handle *badger.DB, likedPostHash BlockHash) (
	_pubKeys [][]byte, _err error) {

	prefix := _dbSeekPrefixForLikerPubKeysLikingAPostHash(likedPostHash)
	keysFound, _ := _enumerateKeysForPrefix(handle, prefix)

	userPubKeys := [][]byte{}
	for _, keyBytes := range keysFound {
		// We must slice off the first byte and likedPostHash to get the userPubKey.
		userPubKey := keyBytes[1+HashSizeBytes:]
		userPubKeys = append(userPubKeys, userPubKey)
	}

	return userPubKeys, nil
}

// -------------------------------------------------------------------------------------
// Reclouts mapping functions
// 		<prefix, user pub key [33]byte, reclouted post BlockHash> -> <>
// 		<prefix, reclouted post BlockHash, user pub key [33]byte> -> <>
// -------------------------------------------------------------------------------------
//_PrefixReclouterPubKeyRecloutedPostHashToRecloutPostHash
func _dbKeyForReclouterPubKeyRecloutedPostHashToRecloutPostHash(userPubKey []byte, recloutedPostHash BlockHash) []byte {
	// Make a copy to avoid multiple calls to this function re-using the same slice.
	prefixCopy := append([]byte{}, _PrefixReclouterPubKeyRecloutedPostHashToRecloutPostHash...)
	key := append(prefixCopy, userPubKey...)
	key = append(key, recloutedPostHash[:]...)
	return key
}

func _dbSeekPrefixForPostHashesYouReclout(yourPubKey []byte) []byte {
	// Make a copy to avoid multiple calls to this function re-using the same slice.
	prefixCopy := append([]byte{}, _PrefixReclouterPubKeyRecloutedPostHashToRecloutPostHash...)
	return append(prefixCopy, yourPubKey...)
}

// Note that this adds a mapping for the user *and* the reclouted post.
func DbPutRecloutMappingsWithTxn(
	txn *badger.Txn, userPubKey []byte, recloutedPostHash BlockHash, recloutEntry RecloutEntry) error {

	if len(userPubKey) != btcec.PubKeyBytesLenCompressed {
		return fmt.Errorf("DbPutRecloutMappingsWithTxn: User public key "+
			"length %d != %d", len(userPubKey), btcec.PubKeyBytesLenCompressed)
	}

	recloutDataBuf := bytes.NewBuffer([]byte{})
	gob.NewEncoder(recloutDataBuf).Encode(recloutEntry)

	if err := txn.Set(_dbKeyForReclouterPubKeyRecloutedPostHashToRecloutPostHash(
		userPubKey, recloutedPostHash), recloutDataBuf.Bytes()); err != nil {

		return errors.Wrapf(
			err, "DbPutRecloutMappingsWithTxn: Problem adding user to reclouted post mapping: ")
	}

	return nil
}

func DbPutRecloutMappings(
	handle *badger.DB, userPubKey []byte, recloutedPostHash BlockHash, recloutEntry RecloutEntry) error {

	return handle.Update(func(txn *badger.Txn) error {
		return DbPutRecloutMappingsWithTxn(txn, userPubKey, recloutedPostHash, recloutEntry)
	})
}

func DbGetReclouterPubKeyRecloutedPostHashToRecloutEntryWithTxn(
	txn *badger.Txn, userPubKey []byte, recloutedPostHash BlockHash) *RecloutEntry {

	key := _dbKeyForReclouterPubKeyRecloutedPostHashToRecloutPostHash(userPubKey, recloutedPostHash)
	recloutEntryObj := &RecloutEntry{}
	recloutEntryItem, err := txn.Get(key)
	if err != nil {
		return nil
	}
	err = recloutEntryItem.Value(func(valBytes []byte) error {
		return gob.NewDecoder(bytes.NewReader(valBytes)).Decode(recloutEntryObj)
	})
	if err != nil {
		glog.Errorf("DbGetReclouterPubKeyRecloutedPostHashToRecloutedPostMappingWithTxn: Problem reading "+
			"RecloutEntry for postHash %v", recloutedPostHash)
		return nil
	}
	return recloutEntryObj
}

func DbReclouterPubKeyRecloutedPostHashToRecloutEntry(
	db *badger.DB, userPubKey []byte, recloutedPostHash BlockHash) *RecloutEntry {
	var ret *RecloutEntry
	db.View(func(txn *badger.Txn) error {
		ret = DbGetReclouterPubKeyRecloutedPostHashToRecloutEntryWithTxn(txn, userPubKey, recloutedPostHash)
		return nil
	})
	return ret
}

// Note this deletes the reclout for the user *and* the reclouted post since a mapping
// should exist for each.
func DbDeleteRecloutMappingsWithTxn(
	txn *badger.Txn, userPubKey []byte, recloutedPostHash BlockHash) error {

	// First check that a mapping exists. If one doesn't exist then there's nothing to do.
	existingMapping := DbGetReclouterPubKeyRecloutedPostHashToRecloutEntryWithTxn(
		txn, userPubKey, recloutedPostHash)
	if existingMapping == nil {
		return nil
	}

	// When a reclout exists, delete the reclout entry mapping.
	if err := txn.Delete(_dbKeyForReclouterPubKeyRecloutedPostHashToRecloutPostHash(userPubKey, recloutedPostHash)); err != nil {
		return errors.Wrapf(err, "DbDeleteRecloutMappingsWithTxn: Deleting "+
			"user public key %s and reclouted post hash %s failed",
			PkToStringMainnet(userPubKey[:]), PkToStringMainnet(recloutedPostHash[:]))
	}
	return nil
}

func DbDeleteRecloutMappings(
	handle *badger.DB, userPubKey []byte, recloutedPostHash BlockHash) error {
	return handle.Update(func(txn *badger.Txn) error {
		return DbDeleteRecloutMappingsWithTxn(txn, userPubKey, recloutedPostHash)
	})
}

func DbGetPostHashesYouReclout(handle *badger.DB, yourPublicKey []byte) (
	_postHashes []*BlockHash, _err error) {

	prefix := _dbSeekPrefixForPostHashesYouReclout(yourPublicKey)
	keysFound, _ := _enumerateKeysForPrefix(handle, prefix)

	postHashesYouReclout := []*BlockHash{}
	for _, keyBytes := range keysFound {
		// We must slice off the first byte and userPubKey to get the recloutedPostHash.
		postHash := &BlockHash{}
		copy(postHash[:], keyBytes[1+btcec.PubKeyBytesLenCompressed:])
		postHashesYouReclout = append(postHashesYouReclout, postHash)
	}

	return postHashesYouReclout, nil
}

// -------------------------------------------------------------------------------------
// Follows mapping functions
// 		<prefix, follower pub key [33]byte, followed pub key [33]byte> -> <>
// 		<prefix, followed pub key [33]byte, follower pub key [33]byte> -> <>
// -------------------------------------------------------------------------------------

func _dbKeyForFollowerToFollowedMapping(
	followerPKID *PKID, followedPKID *PKID) []byte {
	// Make a copy to avoid multiple calls to this function re-using the same slice.
	prefixCopy := append([]byte{}, _PrefixFollowerPKIDToFollowedPKID...)
	key := append(prefixCopy, followerPKID[:]...)
	key = append(key, followedPKID[:]...)
	return key
}

func _dbKeyForFollowedToFollowerMapping(
	followedPKID *PKID, followerPKID *PKID) []byte {
	// Make a copy to avoid multiple calls to this function re-using the same slice.
	prefixCopy := append([]byte{}, _PrefixFollowedPKIDToFollowerPKID...)
	key := append(prefixCopy, followedPKID[:]...)
	key = append(key, followerPKID[:]...)
	return key
}

func _dbSeekPrefixForPKIDsYouFollow(yourPKID *PKID) []byte {
	// Make a copy to avoid multiple calls to this function re-using the same slice.
	prefixCopy := append([]byte{}, _PrefixFollowerPKIDToFollowedPKID...)
	return append(prefixCopy, yourPKID[:]...)
}

func _dbSeekPrefixForPKIDsFollowingYou(yourPKID *PKID) []byte {
	// Make a copy to avoid multiple calls to this function re-using the same slice.
	prefixCopy := append([]byte{}, _PrefixFollowedPKIDToFollowerPKID...)
	return append(prefixCopy, yourPKID[:]...)
}

// Note that this adds a mapping for the follower *and* the pub key being followed.
func DbPutFollowMappingsWithTxn(
	txn *badger.Txn, followerPKID *PKID, followedPKID *PKID) error {

	if len(followerPKID) != btcec.PubKeyBytesLenCompressed {
		return fmt.Errorf("DbPutFollowMappingsWithTxn: Follower PKID "+
			"length %d != %d", len(followerPKID[:]), btcec.PubKeyBytesLenCompressed)
	}
	if len(followedPKID) != btcec.PubKeyBytesLenCompressed {
		return fmt.Errorf("DbPutFollowMappingsWithTxn: Followed PKID "+
			"length %d != %d", len(followerPKID), btcec.PubKeyBytesLenCompressed)
	}

	if err := txn.Set(_dbKeyForFollowerToFollowedMapping(
		followerPKID, followedPKID), []byte{}); err != nil {

		return errors.Wrapf(
			err, "DbPutFollowMappingsWithTxn: Problem adding follower to followed mapping: ")
	}
	if err := txn.Set(_dbKeyForFollowedToFollowerMapping(
		followedPKID, followerPKID), []byte{}); err != nil {

		return errors.Wrapf(
			err, "DbPutFollowMappingsWithTxn: Problem adding followed to follower mapping: ")
	}

	return nil
}

func DbPutFollowMappings(
	handle *badger.DB, followerPKID *PKID, followedPKID *PKID) error {

	return handle.Update(func(txn *badger.Txn) error {
		return DbPutFollowMappingsWithTxn(txn, followerPKID, followedPKID)
	})
}

func DbGetFollowerToFollowedMappingWithTxn(
	txn *badger.Txn, followerPKID *PKID, followedPKID *PKID) []byte {

	key := _dbKeyForFollowerToFollowedMapping(followerPKID, followedPKID)
	_, err := txn.Get(key)
	if err != nil {
		return nil
	}

	// Typically we return a DB entry here but we don't store anything for like mappings.
	// We use this function instead of one returning true / false for feature consistency.
	return []byte{}
}

func DbGetFollowerToFollowedMapping(db *badger.DB, followerPKID *PKID, followedPKID *PKID) []byte {
	var ret []byte
	db.View(func(txn *badger.Txn) error {
		ret = DbGetFollowerToFollowedMappingWithTxn(txn, followerPKID, followedPKID)
		return nil
	})
	return ret
}

// Note this deletes the follow for the follower *and* followed since a mapping
// should exist for each.
func DbDeleteFollowMappingsWithTxn(
	txn *badger.Txn, followerPKID *PKID, followedPKID *PKID) error {

	// First check that a mapping exists for the PKIDs passed in.
	// If one doesn't exist then there's nothing to do.
	existingMapping := DbGetFollowerToFollowedMappingWithTxn(
		txn, followerPKID, followedPKID)
	if existingMapping == nil {
		return nil
	}

	// When a message exists, delete the mapping for the sender and receiver.
	if err := txn.Delete(_dbKeyForFollowerToFollowedMapping(followerPKID, followedPKID)); err != nil {
		return errors.Wrapf(err, "DbDeleteFollowMappingsWithTxn: Deleting "+
			"followerPKID %s and followedPKID %s failed",
			PkToStringMainnet(followerPKID[:]), PkToStringMainnet(followedPKID[:]))
	}
	if err := txn.Delete(_dbKeyForFollowedToFollowerMapping(followedPKID, followerPKID)); err != nil {
		return errors.Wrapf(err, "DbDeleteFollowMappingsWithTxn: Deleting "+
			"followedPKID %s and followerPKID %s failed",
			PkToStringMainnet(followedPKID[:]), PkToStringMainnet(followerPKID[:]))
	}

	return nil
}

func DbDeleteFollowMappings(
	handle *badger.DB, followerPKID *PKID, followedPKID *PKID) error {
	return handle.Update(func(txn *badger.Txn) error {
		return DbDeleteFollowMappingsWithTxn(txn, followerPKID, followedPKID)
	})
}

func DbGetPKIDsYouFollow(handle *badger.DB, yourPKID *PKID) (
	_pkids []*PKID, _err error) {

	prefix := _dbSeekPrefixForPKIDsYouFollow(yourPKID)
	keysFound, _ := _enumerateKeysForPrefix(handle, prefix)

	pkidsYouFollow := []*PKID{}
	for _, keyBytes := range keysFound {
		// We must slice off the first byte and followerPKID to get the followedPKID.
		followedPKIDBytes := keyBytes[1+btcec.PubKeyBytesLenCompressed:]
		followedPKID := &PKID{}
		copy(followedPKID[:], followedPKIDBytes)
		pkidsYouFollow = append(pkidsYouFollow, followedPKID)
	}

	return pkidsYouFollow, nil
}

func DbGetPKIDsFollowingYou(handle *badger.DB, yourPKID *PKID) (
	_pkids []*PKID, _err error) {

	prefix := _dbSeekPrefixForPKIDsFollowingYou(yourPKID)
	keysFound, _ := _enumerateKeysForPrefix(handle, prefix)

	pkidsFollowingYou := []*PKID{}
	for _, keyBytes := range keysFound {
		// We must slice off the first byte and followedPKID to get the followerPKID.
		followerPKIDBytes := keyBytes[1+btcec.PubKeyBytesLenCompressed:]
		followerPKID := &PKID{}
		copy(followerPKID[:], followerPKIDBytes)
		pkidsFollowingYou = append(pkidsFollowingYou, followerPKID)
	}

	return pkidsFollowingYou, nil
}

func DbGetPubKeysYouFollow(handle *badger.DB, yourPubKey []byte) (
	_pubKeys [][]byte, _err error) {

	// Get the PKID for the pub key
	yourPKID := DBGetPKIDEntryForPublicKey(handle, yourPubKey)
	followPKIDs, err := DbGetPKIDsYouFollow(handle, yourPKID.PKID)
	if err != nil {
		return nil, errors.Wrap(err, "DbGetPubKeysYouFollow: ")
	}

	// Convert the pkids to public keys
	followPubKeys := [][]byte{}
	for _, fpkid := range followPKIDs {
		followPk := DBGetPublicKeyForPKID(handle, fpkid)
		followPubKeys = append(followPubKeys, followPk)
	}

	return followPubKeys, nil
}

func DbGetPubKeysFollowingYou(handle *badger.DB, yourPubKey []byte) (
	_pubKeys [][]byte, _err error) {

	// Get the PKID for the pub key
	yourPKID := DBGetPKIDEntryForPublicKey(handle, yourPubKey)
	followPKIDs, err := DbGetPKIDsFollowingYou(handle, yourPKID.PKID)
	if err != nil {
		return nil, errors.Wrap(err, "DbGetPubKeysFollowingYou: ")
	}

	// Convert the pkids to public keys
	followPubKeys := [][]byte{}
	for _, fpkid := range followPKIDs {
		followPk := DBGetPublicKeyForPKID(handle, fpkid)
		followPubKeys = append(followPubKeys, followPk)
	}

	return followPubKeys, nil
}

// -------------------------------------------------------------------------------------
// Diamonds mapping functions
//  <prefix, DiamondReceiverPKID [33]byte, DiamondSenderPKID [33]byte, posthash> -> <[]byte{DiamondLevel}>
// -------------------------------------------------------------------------------------

func _dbKeyForDiamondReceiverToDiamondSenderMapping(
	diamondReceiverPKID *PKID, diamondSenderPKID *PKID, diamondPostHash *BlockHash) []byte {
	// Make a copy to avoid multiple calls to this function re-using the same slice.
	prefixCopy := append([]byte{}, _PrefixDiamondReceiverPKIDDiamondSenderPKIDPostHash...)
	key := append(prefixCopy, diamondReceiverPKID[:]...)
	key = append(key, diamondSenderPKID[:]...)
	key = append(key, diamondPostHash[:]...)
	return key
}

func _dbSeekPrefixForPKIDsThatDiamondedYou(yourPKID *PKID) []byte {
	// Make a copy to avoid multiple calls to this function re-using the same slice.
	prefixCopy := append([]byte{}, _PrefixDiamondReceiverPKIDDiamondSenderPKIDPostHash...)
	return append(prefixCopy, yourPKID[:]...)
}

func _dbKeyForDiamondSenderToDiamondRecieverMapping(
	diamondReceiverPKID *PKID, diamondSenderPKID *PKID, diamondPostHash *BlockHash) []byte {
	// Make a copy to avoid multiple calls to this function re-using the same slice.
	prefixCopy := append([]byte{}, _PrefixDiamondSenderPKIDDiamondReciverPKIDPostHash...)
	key := append(prefixCopy, diamondSenderPKID[:]...)
	key = append(key, diamondReceiverPKID[:]...)
	key = append(key, diamondPostHash[:]...)
	return key
}

func _dbSeekPrefixForPKIDsThatYouDiamonded(yourPKID *PKID) []byte {
	// Make a copy to avoid multiple calls to this function re-using the same slice.
	prefixCopy := append([]byte{}, _PrefixDiamondSenderPKIDDiamondReciverPKIDPostHash...)
	return append(prefixCopy, yourPKID[:]...)
}

func _dbSeekPrefixForReceiverPKIDAndSenderPKID(receiverPKID *PKID, senderPKID *PKID) []byte {
	// Make a copy to avoid multiple calls to this function re-using the same slice.
	prefixCopy := append([]byte{}, _PrefixDiamondReceiverPKIDDiamondSenderPKIDPostHash...)
	key := append(prefixCopy, receiverPKID[:]...)
	return append(key, senderPKID[:]...)
}

func _DbBufForDiamondEntry(diamondEntry *DiamondEntry) []byte {
	diamondEntryBuf := bytes.NewBuffer([]byte{})
	gob.NewEncoder(diamondEntryBuf).Encode(diamondEntry)
	return diamondEntryBuf.Bytes()
}

func _DbDiamondEntryForDbBuf(buf []byte) *DiamondEntry {
	if len(buf) == 0 {
		return nil
	}
	ret := &DiamondEntry{}
	if err := gob.NewDecoder(bytes.NewReader(buf)).Decode(&ret); err != nil {
		glog.Errorf("Error decoding DiamondEntry from DB: %v", err)
		return nil
	}
	return ret
}

// Note that this adds a mapping for the follower *and* the pub key being followed.
func DbPutDiamondMappingsWithTxn(
	txn *badger.Txn,
	diamondEntry *DiamondEntry) error {

	if len(diamondEntry.ReceiverPKID) != btcec.PubKeyBytesLenCompressed {
		return fmt.Errorf("DbPutDiamondMappingsWithTxn: Receiver PKID "+
			"length %d != %d", len(diamondEntry.ReceiverPKID[:]), btcec.PubKeyBytesLenCompressed)
	}
	if len(diamondEntry.SenderPKID) != btcec.PubKeyBytesLenCompressed {
		return fmt.Errorf("DbPutDiamondMappingsWithTxn: Sender PKID "+
			"length %d != %d", len(diamondEntry.SenderPKID), btcec.PubKeyBytesLenCompressed)
	}
	diamondEntryBytes := _DbBufForDiamondEntry(diamondEntry)
	if err := txn.Set(_dbKeyForDiamondReceiverToDiamondSenderMapping(
		diamondEntry.ReceiverPKID, diamondEntry.SenderPKID, diamondEntry.DiamondPostHash),
		diamondEntryBytes); err != nil {

		return errors.Wrapf(
			err, "DbPutDiamondMappingsWithTxn: Problem adding receiver to giver mapping: ")
	}

	if err := txn.Set(_dbKeyForDiamondSenderToDiamondRecieverMapping(
		diamondEntry.ReceiverPKID, diamondEntry.SenderPKID, diamondEntry.DiamondPostHash),
		diamondEntryBytes); err != nil {
		return errors.Wrapf(err, "DbPutDiamondMappingsWithTxn: Problem adding sender to receiver mapping: ")
	}

	return nil
}

func DbPutDiamondMappings(
	handle *badger.DB,
	diamondEntry *DiamondEntry) error {

	return handle.Update(func(txn *badger.Txn) error {
		return DbPutDiamondMappingsWithTxn(
			txn, diamondEntry)
	})
}

func DbGetDiamondMappingsWithTxn(
	txn *badger.Txn, diamondReceiverPKID *PKID, diamondSenderPKID *PKID, diamondPostHash *BlockHash) *DiamondEntry {

	key := _dbKeyForDiamondReceiverToDiamondSenderMapping(diamondReceiverPKID, diamondSenderPKID, diamondPostHash)
	item, err := txn.Get(key)
	if err != nil {
		return nil
	}

	diamondEntryBuf, err := item.ValueCopy(nil)
	if err != nil {
		return nil
	}

	// We return the byte array stored for this diamond mapping. This mapping should only
	// hold one uint8 with a value between 1 and 5 but the caller is responsible for sanity
	// checking in order to maintain consistency with other DB functions that do not error.
	return _DbDiamondEntryForDbBuf(diamondEntryBuf)
}

func DbGetDiamondMappings(
	db *badger.DB, diamondReceiverPKID *PKID, diamondSenderPKID *PKID, diamondPostHash *BlockHash) *DiamondEntry {
	var ret *DiamondEntry
	db.View(func(txn *badger.Txn) error {
		ret = DbGetDiamondMappingsWithTxn(
			txn, diamondReceiverPKID, diamondSenderPKID, diamondPostHash)
		return nil
	})
	return ret
}

// This currently only deletes one index mapping for diamonds. However, we will likely
// add additional index mappings in the future.
func DbDeleteDiamondMappingsWithTxn(
	txn *badger.Txn, diamondReceiverPKID *PKID, diamondSenderPKID *PKID, diamondPostHash *BlockHash) error {

	// First check that a mapping exists for the PKIDs passed in.
	// If one doesn't exist then there's nothing to do.
	existingMapping := DbGetDiamondMappingsWithTxn(
		txn, diamondReceiverPKID, diamondSenderPKID, diamondPostHash)
	if existingMapping == nil {
		return nil
	}

	// When a DiamondEntry exists, delete the mapping.
	if err := txn.Delete(_dbKeyForDiamondReceiverToDiamondSenderMapping(
		diamondReceiverPKID, diamondSenderPKID, diamondPostHash)); err != nil {
		return errors.Wrapf(err, "DbDeleteDiamondMappingsWithTxn: Deleting "+
			"diamondReceiverPKID %s and diamondSenderPKID %s and diamondPostHash %s failed",
			PkToStringMainnet(diamondReceiverPKID[:]),
			PkToStringMainnet(diamondSenderPKID[:]),
			diamondPostHash.String(),
		)
	}

	if err := txn.Delete(_dbKeyForDiamondSenderToDiamondRecieverMapping(
		diamondReceiverPKID, diamondSenderPKID, diamondPostHash)); err != nil {
		return errors.Wrapf(err, "DbDeleteDiamondMappingsWithTxn: Deleting "+
			"diamondSenderPKID %s and diamondReceiverPKID %s and diamondPostHash %s failed",
			PkToStringMainnet(diamondSenderPKID[:]),
			PkToStringMainnet(diamondReceiverPKID[:]),
			diamondPostHash.String(),
		)
	}

	return nil
}

func DbDeleteDiamondMappings(
	handle *badger.DB, diamondReceiverPKID *PKID, diamondGiverPKID *PKID, diamondPostHash *BlockHash) error {
	return handle.Update(func(txn *badger.Txn) error {
		return DbDeleteDiamondMappingsWithTxn(txn, diamondReceiverPKID, diamondGiverPKID, diamondPostHash)
	})
}

// This function returns a map of PKIDs that gave diamonds to a list of DiamondEntrys
// that contain post hashes.
func DbGetPKIDsThatDiamondedYouMap(handle *badger.DB, yourPKID *PKID, fetchYouDiamonded bool) (
	_pkidToDiamondsMap map[PKID][]*DiamondEntry, _err error) {

	prefix := _dbSeekPrefixForPKIDsThatDiamondedYou(yourPKID)
	diamondSenderStartIdx := 1 + btcec.PubKeyBytesLenCompressed
	diamondSenderEndIdx := 1 + 2*btcec.PubKeyBytesLenCompressed
	diamondReceiverStartIdx := 1
	diamondReceiverEndIdx := 1 + btcec.PubKeyBytesLenCompressed
	if fetchYouDiamonded {
		prefix = _dbSeekPrefixForPKIDsThatYouDiamonded(yourPKID)
		diamondSenderStartIdx = 1
		diamondSenderEndIdx = 1 + btcec.PubKeyBytesLenCompressed
		diamondReceiverStartIdx = 1 + btcec.PubKeyBytesLenCompressed
		diamondReceiverEndIdx = 1 + 2*btcec.PubKeyBytesLenCompressed
	}
	keysFound, valsFound := _enumerateKeysForPrefix(handle, prefix)

	pkidsToDiamondEntryMap := make(map[PKID][]*DiamondEntry)
	for ii, keyBytes := range keysFound {
		// The DiamondEntry found must not be nil.
		diamondEntry := _DbDiamondEntryForDbBuf(valsFound[ii])
		if diamondEntry == nil {
			return nil, fmt.Errorf(
				"DbGetPKIDsThatDiamondedYouMap: Found nil DiamondEntry for public key %v "+
					"and key bytes %#v when seeking; this should never happen",
				PkToStringMainnet(yourPKID[:]), keyBytes)
		}
		expectedDiamondKeyLen := 1 + 2*btcec.PubKeyBytesLenCompressed + HashSizeBytes
		if len(keyBytes) != expectedDiamondKeyLen {
			return nil, fmt.Errorf(
				"DbGetPKIDsThatDiamondedYouMap: Invalid key length %v should be %v",
				len(keyBytes), expectedDiamondKeyLen)
		}

		// Note: The code below is mainly just sanity-checking. Checking the key isn't actually
		// needed in this function, since all the information is duplicated in the entry.

		// Chop out the diamond sender PKID.
		diamondSenderPKIDBytes := keyBytes[diamondSenderStartIdx:diamondSenderEndIdx]
		diamondSenderPKID := &PKID{}
		copy(diamondSenderPKID[:], diamondSenderPKIDBytes)
		// It must match what's in the DiamondEntry
		if !reflect.DeepEqual(diamondSenderPKID, diamondEntry.SenderPKID) {
			return nil, fmt.Errorf(
				"DbGetPKIDsThatDiamondedYouMap: Sender PKID in DB %v did not "+
					"match Sender PKID in DiamondEntry %v; this should never happen",
				PkToStringBoth(diamondSenderPKID[:]), PkToStringBoth(diamondEntry.SenderPKID[:]))
		}

		// Chop out the diamond receiver PKID
		diamondReceiverPKIDBytes := keyBytes[diamondReceiverStartIdx:diamondReceiverEndIdx]
		diamondReceiverPKID := &PKID{}
		copy(diamondReceiverPKID[:], diamondReceiverPKIDBytes)
		// It must match what's in the DiamondEntry
		if !reflect.DeepEqual(diamondReceiverPKID, diamondEntry.ReceiverPKID) {
			return nil, fmt.Errorf(
				"DbGetPKIDsThatDiamondedYouMap: Receiver PKID in DB %v did not "+
					"match Receiver PKID in DiamondEntry %v; this should never happen",
				PkToStringBoth(diamondReceiverPKID[:]), PkToStringBoth(diamondEntry.ReceiverPKID[:]))
		}

		// Chop out the diamond post hash.
		diamondPostHashBytes := keyBytes[1+2*btcec.PubKeyBytesLenCompressed:]
		diamondPostHash := &BlockHash{}
		copy(diamondPostHash[:], diamondPostHashBytes)
		// It must match what's in the entry
		if *diamondPostHash != *diamondEntry.DiamondPostHash {
			return nil, fmt.Errorf(
				"DbGetPKIDsThatDiamondedYouMap: Post hash found in DB key %v "+
					"did not match post hash in DiamondEntry %v; this should never happen",
				diamondPostHash, diamondEntry.DiamondPostHash)
		}

		// If a map entry doesn't exist for this sender, create one.
		newListOfEntrys := pkidsToDiamondEntryMap[*diamondSenderPKID]
		newListOfEntrys = append(newListOfEntrys, diamondEntry)
		pkidsToDiamondEntryMap[*diamondSenderPKID] = newListOfEntrys
	}

	return pkidsToDiamondEntryMap, nil
}

// This function returns a list of DiamondEntrys given by giverPKID to receiverPKID that contain post hashes.
func DbGetDiamondEntriesForSenderToReceiver(handle *badger.DB, receiverPKID *PKID, senderPKID *PKID) (
	_diamondEntries []*DiamondEntry, _err error) {

	prefix := _dbSeekPrefixForReceiverPKIDAndSenderPKID(receiverPKID, senderPKID)
	keysFound, valsFound := _enumerateKeysForPrefix(handle, prefix)
	var diamondEntries []*DiamondEntry
	for ii, keyBytes := range keysFound {
		// The DiamondEntry found must not be nil.
		diamondEntry := _DbDiamondEntryForDbBuf(valsFound[ii])
		if diamondEntry == nil {
			return nil, fmt.Errorf(
				"DbGetDiamondEntriesForGiverToReceiver: Found nil DiamondEntry for receiver key %v "+
					"and giver key %v when seeking; this should never happen",
				PkToStringMainnet(receiverPKID[:]), PkToStringMainnet(senderPKID[:]))
		}
		expectedDiamondKeyLen := 1 + 2*btcec.PubKeyBytesLenCompressed + HashSizeBytes
		if len(keyBytes) != expectedDiamondKeyLen {
			return nil, fmt.Errorf(
				"DbGetDiamondEntriesForGiverToReceiver: Invalid key length %v should be %v",
				len(keyBytes), expectedDiamondKeyLen)
		}

		// Note: The code below is mainly just sanity-checking. Checking the key isn't actually
		// needed in this function, since all the information is duplicated in the entry.

		// Chop out the diamond sender PKID.
		diamondSenderPKIDBytes := keyBytes[1+btcec.PubKeyBytesLenCompressed : 1+2*btcec.PubKeyBytesLenCompressed]
		diamondSenderPKID := &PKID{}
		copy(diamondSenderPKID[:], diamondSenderPKIDBytes)
		// It must match what's in the DiamondEntry
		if !reflect.DeepEqual(diamondSenderPKID, diamondEntry.SenderPKID) {
			return nil, fmt.Errorf(
				"DbGetDiamondEntriesForGiverToReceiver: Sender PKID in DB %v did not "+
					"match Sender PKID in DiamondEntry %v; this should never happen",
				PkToStringBoth(diamondSenderPKID[:]), PkToStringBoth(diamondEntry.SenderPKID[:]))
		}

		// Chop out the diamond post hash.
		diamondPostHashBytes := keyBytes[1+2*btcec.PubKeyBytesLenCompressed:]
		diamondPostHash := &BlockHash{}
		copy(diamondPostHash[:], diamondPostHashBytes)
		// It must match what's in the entry
		if *diamondPostHash != *diamondEntry.DiamondPostHash {
			return nil, fmt.Errorf(
				"DbGetDiamondEntriesForGiverToReceiver: Post hash found in DB key %v "+
					"did not match post hash in DiamondEntry %v; this should never happen",
				diamondPostHash, diamondEntry.DiamondPostHash)
		}
		// Append the diamond entry to the slice
		diamondEntries = append(diamondEntries, diamondEntry)
	}
	return diamondEntries, nil
}

// -------------------------------------------------------------------------------------
// BitcoinBurnTxID mapping functions
// <BitcoinBurnTxID BlockHash> -> <>
// -------------------------------------------------------------------------------------

func _keyForBitcoinBurnTxID(bitcoinBurnTxID *BlockHash) []byte {
	// Make a copy to avoid multiple calls to this function re-using the same
	// underlying array.
	prefixCopy := append([]byte{}, _PrefixBitcoinBurnTxIDs...)
	return append(prefixCopy, bitcoinBurnTxID[:]...)
}

func DbPutBitcoinBurnTxIDWithTxn(txn *badger.Txn, bitcoinBurnTxID *BlockHash) error {
	return txn.Set(_keyForBitcoinBurnTxID(bitcoinBurnTxID), []byte{})
}

func DbExistsBitcoinBurnTxIDWithTxn(txn *badger.Txn, bitcoinBurnTxID *BlockHash) bool {
	// We don't care about the value because we're just checking to see if the key exists.
	if _, err := txn.Get(_keyForBitcoinBurnTxID(bitcoinBurnTxID)); err != nil {
		return false
	}
	return true
}

func DbExistsBitcoinBurnTxID(db *badger.DB, bitcoinBurnTxID *BlockHash) bool {
	var exists bool
	db.View(func(txn *badger.Txn) error {
		exists = DbExistsBitcoinBurnTxIDWithTxn(txn, bitcoinBurnTxID)
		return nil
	})
	return exists
}

func DbDeleteBitcoinBurnTxIDWithTxn(txn *badger.Txn, bitcoinBurnTxID *BlockHash) error {
	return txn.Delete(_keyForBitcoinBurnTxID(bitcoinBurnTxID))
}

func DbGetAllBitcoinBurnTxIDs(handle *badger.DB) (_bitcoinBurnTxIDs []*BlockHash) {
	keysFound, _ := _enumerateKeysForPrefix(handle, _PrefixBitcoinBurnTxIDs)
	bitcoinBurnTxIDs := []*BlockHash{}
	for _, key := range keysFound {
		bbtxid := &BlockHash{}
		copy(bbtxid[:], key[1:])
		bitcoinBurnTxIDs = append(bitcoinBurnTxIDs, bbtxid)
	}

	return bitcoinBurnTxIDs
}

func _getBlockHashForPrefixWithTxn(txn *badger.Txn, prefix []byte) *BlockHash {
	var ret BlockHash
	bhItem, err := txn.Get(prefix)
	if err != nil {
		return nil
	}
	_, err = bhItem.ValueCopy(ret[:])
	if err != nil {
		return nil
	}

	return &ret
}

func _getBlockHashForPrefix(handle *badger.DB, prefix []byte) *BlockHash {
	var ret *BlockHash
	err := handle.View(func(txn *badger.Txn) error {
		ret = _getBlockHashForPrefixWithTxn(txn, prefix)
		return nil
	})
	if err != nil {
		return nil
	}
	return ret
}

// GetBadgerDbPath returns the path where we store the badgerdb data.
func GetBadgerDbPath(dataDir string) string {
	return filepath.Join(dataDir, badgerDbFolder)
}

func _EncodeUint32(num uint32) []byte {
	numBytes := make([]byte, 4)
	binary.BigEndian.PutUint32(numBytes, num)
	return numBytes
}

func DecodeUint32(num []byte) uint32 {
	return binary.BigEndian.Uint32(num)
}

func EncodeUint64(num uint64) []byte {
	numBytes := make([]byte, 8)
	binary.BigEndian.PutUint64(numBytes, num)
	return numBytes
}

func DecodeUint64(scoreBytes []byte) uint64 {
	return binary.BigEndian.Uint64(scoreBytes)
}

func DbPutNanosPurchasedWithTxn(txn *badger.Txn, nanosPurchased uint64) error {
	return txn.Set(_KeyNanosPurchased, EncodeUint64(nanosPurchased))
}

func DbPutNanosPurchased(handle *badger.DB, nanosPurchased uint64) error {
	return handle.Update(func(txn *badger.Txn) error {
		return DbPutNanosPurchasedWithTxn(txn, nanosPurchased)
	})
}

func DbGetNanosPurchasedWithTxn(txn *badger.Txn) uint64 {
	nanosPurchasedItem, err := txn.Get(_KeyNanosPurchased)
	if err != nil {
		return 0
	}
	nanosPurchasedBuf, err := nanosPurchasedItem.ValueCopy(nil)
	if err != nil {
		return 0
	}

	return DecodeUint64(nanosPurchasedBuf)
}

func DbGetNanosPurchased(handle *badger.DB) uint64 {
	var nanosPurchased uint64
	handle.View(func(txn *badger.Txn) error {
		nanosPurchased = DbGetNanosPurchasedWithTxn(txn)
		return nil
	})

	return nanosPurchased
}

func DbPutGlobalParamsEntry(handle *badger.DB, globalParamsEntry GlobalParamsEntry) error {
	return handle.Update(func(txn *badger.Txn) error {
		return DbPutGlobalParamsEntryWithTxn(txn, globalParamsEntry)
	})
}

func DbPutGlobalParamsEntryWithTxn(txn *badger.Txn, globalParamsEntry GlobalParamsEntry) error {
	globalParamsDataBuf := bytes.NewBuffer([]byte{})
	err := gob.NewEncoder(globalParamsDataBuf).Encode(globalParamsEntry)
	if err != nil {
		return errors.Wrapf(err, "DbPutGlobalParamsEntryWithTxn: Problem encoding global params entry: ")
	}

	err = txn.Set(_KeyGlobalParams, globalParamsDataBuf.Bytes())
	if err != nil {
		return errors.Wrapf(err, "DbPutGlobalParamsEntryWithTxn: Problem adding global params entry to db: ")
	}
	return nil
}

func DbGetGlobalParamsEntryWithTxn(txn *badger.Txn) *GlobalParamsEntry {
	globalParamsEntryItem, err := txn.Get(_KeyGlobalParams)
	if err != nil {
		return &InitialGlobalParamsEntry
	}
	globalParamsEntryObj := &GlobalParamsEntry{}
	err = globalParamsEntryItem.Value(func(valBytes []byte) error {
		return gob.NewDecoder(bytes.NewReader(valBytes)).Decode(globalParamsEntryObj)
	})
	if err != nil {
		glog.Errorf("DbGetGlobalParamsEntryWithTxn: Problem reading "+
			"GlobalParamsEntry: %v", err)
		return &InitialGlobalParamsEntry
	}

	return globalParamsEntryObj
}

func DbGetGlobalParamsEntry(handle *badger.DB) *GlobalParamsEntry {
	var globalParamsEntry *GlobalParamsEntry
	handle.View(func(txn *badger.Txn) error {
		globalParamsEntry = DbGetGlobalParamsEntryWithTxn(txn)
		return nil
	})
	return globalParamsEntry
}

func DbPutUSDCentsPerBitcoinExchangeRateWithTxn(txn *badger.Txn, usdCentsPerBitcoinExchangeRate uint64) error {
	return txn.Set(_KeyUSDCentsPerBitcoinExchangeRate, EncodeUint64(usdCentsPerBitcoinExchangeRate))
}

func DbGetUSDCentsPerBitcoinExchangeRateWithTxn(txn *badger.Txn) uint64 {
	usdCentsPerBitcoinExchangeRateItem, err := txn.Get(_KeyUSDCentsPerBitcoinExchangeRate)
	if err != nil {
		return InitialUSDCentsPerBitcoinExchangeRate
	}
	usdCentsPerBitcoinExchangeRateBuf, err := usdCentsPerBitcoinExchangeRateItem.ValueCopy(nil)
	if err != nil {
		glog.Error("DbGetUSDCentsPerBitcoinExchangeRateWithTxn: Error parsing DB " +
			"value; this shouldn't really happen ever")
		return InitialUSDCentsPerBitcoinExchangeRate
	}

	return DecodeUint64(usdCentsPerBitcoinExchangeRateBuf)
}

func DbGetUSDCentsPerBitcoinExchangeRate(handle *badger.DB) uint64 {
	var usdCentsPerBitcoinExchangeRate uint64
	handle.View(func(txn *badger.Txn) error {
		usdCentsPerBitcoinExchangeRate = DbGetUSDCentsPerBitcoinExchangeRateWithTxn(txn)
		return nil
	})

	return usdCentsPerBitcoinExchangeRate
}

func GetUtxoNumEntriesWithTxn(txn *badger.Txn) uint64 {
	indexItem, err := txn.Get(_KeyUtxoNumEntries)
	if err != nil {
		return 0
	}
	// Get the current index.
	indexBytes, err := indexItem.ValueCopy(nil)
	if err != nil {
		return 0
	}
	numEntries := DecodeUint64(indexBytes)

	return numEntries
}

func GetUtxoNumEntries(handle *badger.DB) uint64 {
	var numEntries uint64
	handle.View(func(txn *badger.Txn) error {
		numEntries = GetUtxoNumEntriesWithTxn(txn)

		return nil
	})

	return numEntries
}

func _SerializeUtxoKey(utxoKey *UtxoKey) []byte {
	indexBytes := make([]byte, 4)
	binary.BigEndian.PutUint32(indexBytes, utxoKey.Index)
	return append(utxoKey.TxID[:], indexBytes...)

}

func _DbKeyForUtxoKey(utxoKey *UtxoKey) []byte {
	return append(append([]byte{}, _PrefixUtxoKeyToUtxoEntry...), _SerializeUtxoKey(utxoKey)...)
}

// Implements the reverse of _DbKeyForUtxoKey. This doesn't error-check
// and caller should make sure they're passing a properly-sized key to
// this function.
func _UtxoKeyFromDbKey(utxoDbKey []byte) *UtxoKey {
	// Read in the TxID, which is at the beginning.
	txIDBytes := utxoDbKey[:HashSizeBytes]
	txID := BlockHash{}
	copy(txID[:], txIDBytes)
	// Read in the index, which is encoded as a bigint at the end.
	indexBytes := utxoDbKey[HashSizeBytes:]
	indexValue := binary.BigEndian.Uint32(indexBytes)
	return &UtxoKey{
		Index: indexValue,
		TxID:  txID,
	}
}

func _DbBufForUtxoEntry(utxoEntry *UtxoEntry) []byte {
	utxoEntryBuf := bytes.NewBuffer([]byte{})
	gob.NewEncoder(utxoEntryBuf).Encode(utxoEntry)
	return utxoEntryBuf.Bytes()
}

func PutUtxoNumEntriesWithTxn(txn *badger.Txn, newNumEntries uint64) error {
	return txn.Set(_KeyUtxoNumEntries, EncodeUint64(newNumEntries))
}

func PutUtxoEntryForUtxoKeyWithTxn(txn *badger.Txn, utxoKey *UtxoKey, utxoEntry *UtxoEntry) error {
	return txn.Set(_DbKeyForUtxoKey(utxoKey), _DbBufForUtxoEntry(utxoEntry))
}

func DbGetUtxoEntryForUtxoKeyWithTxn(txn *badger.Txn, utxoKey *UtxoKey) *UtxoEntry {
	var ret UtxoEntry
	utxoDbKey := _DbKeyForUtxoKey(utxoKey)
	item, err := txn.Get(utxoDbKey)
	if err != nil {
		return nil
	}

	err = item.Value(func(valBytes []byte) error {
		// TODO: Storing with gob is very slow due to reflection. Would be
		// better if we serialized/deserialized manually.
		if err := gob.NewDecoder(bytes.NewReader(valBytes)).Decode(&ret); err != nil {
			return err
		}

		return nil
	})

	if err != nil {
		return nil
	}

	return &ret
}

func DbGetUtxoEntryForUtxoKey(handle *badger.DB, utxoKey *UtxoKey) *UtxoEntry {
	var ret *UtxoEntry
	handle.View(func(txn *badger.Txn) error {
		ret = DbGetUtxoEntryForUtxoKeyWithTxn(txn, utxoKey)
		return nil
	})

	return ret
}

func DeleteUtxoEntryForKeyWithTxn(txn *badger.Txn, utxoKey *UtxoKey) error {
	return txn.Delete(_DbKeyForUtxoKey(utxoKey))
}

func DeletePubKeyUtxoKeyMappingWithTxn(txn *badger.Txn, publicKey []byte, utxoKey *UtxoKey) error {
	if len(publicKey) != btcec.PubKeyBytesLenCompressed {
		return fmt.Errorf("DeletePubKeyUtxoKeyMappingWithTxn: Public key has improper length %d != %d", len(publicKey), btcec.PubKeyBytesLenCompressed)
	}

	keyToDelete := append(append([]byte{}, _PrefixPubKeyUtxoKey...), publicKey...)
	keyToDelete = append(keyToDelete, _SerializeUtxoKey(utxoKey)...)

	return txn.Delete(keyToDelete)
}

func DbBufForUtxoKey(utxoKey *UtxoKey) []byte {
	utxoKeyBuf := bytes.NewBuffer([]byte{})
	gob.NewEncoder(utxoKeyBuf).Encode(utxoKey)
	return utxoKeyBuf.Bytes()
}

func PutPubKeyUtxoKeyWithTxn(txn *badger.Txn, publicKey []byte, utxoKey *UtxoKey) error {
	if len(publicKey) != btcec.PubKeyBytesLenCompressed {
		return fmt.Errorf("PutPubKeyUtxoKeyWithTxn: Public key has improper length %d != %d", len(publicKey), btcec.PubKeyBytesLenCompressed)
	}

	keyToAdd := append(append([]byte{}, _PrefixPubKeyUtxoKey...), publicKey...)
	keyToAdd = append(keyToAdd, _SerializeUtxoKey(utxoKey)...)

	return txn.Set(keyToAdd, []byte{})
}

// DbGetUtxosForPubKey finds the UtxoEntry's corresponding to the public
// key passed in. It also attaches the UtxoKeys to the UtxoEntry's it
// returns for easy access.
func DbGetUtxosForPubKey(publicKey []byte, handle *badger.DB) ([]*UtxoEntry, error) {
	// Verify the length of the public key.
	if len(publicKey) != btcec.PubKeyBytesLenCompressed {
		return nil, fmt.Errorf("DbGetUtxosForPubKey: Public key has improper "+
			"length %d != %d", len(publicKey), btcec.PubKeyBytesLenCompressed)
	}
	// Look up the utxo keys for this public key.
	utxoEntriesFound := []*UtxoEntry{}
	err := handle.View(func(txn *badger.Txn) error {
		// Start by looping through to find all the UtxoKeys.
		utxoKeysFound := []*UtxoKey{}
		opts := badger.DefaultIteratorOptions
		nodeIterator := txn.NewIterator(opts)
		defer nodeIterator.Close()
		prefix := append(append([]byte{}, _PrefixPubKeyUtxoKey...), publicKey...)
		for nodeIterator.Seek(prefix); nodeIterator.ValidForPrefix(prefix); nodeIterator.Next() {
			// Strip the prefix off the key. What's left should be the UtxoKey.
			pkUtxoKey := nodeIterator.Item().Key()
			utxoKeyBytes := pkUtxoKey[len(prefix):]
			// The size of the utxo key bytes should be equal to the size of a
			// standard hash (the txid) plus the size of a uint32.
			if len(utxoKeyBytes) != HashSizeBytes+4 {
				return fmt.Errorf("Problem reading <pk, utxoKey> mapping; key size %d "+
					"is not equal to (prefix_byte=%d + len(publicKey)=%d + len(utxoKey)=%d)=%d. "+
					"Key found: %#v", len(pkUtxoKey), len(_PrefixPubKeyUtxoKey), len(publicKey), HashSizeBytes+4, len(prefix)+HashSizeBytes+4, pkUtxoKey)
			}
			// Try and convert the utxo key bytes into a utxo key.
			utxoKey := _UtxoKeyFromDbKey(utxoKeyBytes)
			if utxoKey == nil {
				return fmt.Errorf("Problem reading <pk, utxoKey> mapping; parsing UtxoKey bytes %#v returned nil", utxoKeyBytes)
			}

			// Now that we have the utxoKey, enqueue it.
			utxoKeysFound = append(utxoKeysFound, utxoKey)
		}

		// Once all the UtxoKeys are found, fetch all the UtxoEntries.
		for ii := range utxoKeysFound {
			foundUtxoKey := utxoKeysFound[ii]
			utxoEntry := DbGetUtxoEntryForUtxoKeyWithTxn(txn, foundUtxoKey)
			if utxoEntry == nil {
				return fmt.Errorf("UtxoEntry for UtxoKey %v was not found", foundUtxoKey)
			}

			// Set a back-reference to the utxo key.
			utxoEntry.UtxoKey = foundUtxoKey

			utxoEntriesFound = append(utxoEntriesFound, utxoEntry)
		}

		return nil
	})
	if err != nil {
		return nil, errors.Wrapf(err, "DbGetUtxosForPubKey: ")
	}

	// If there are no errors, return everything we found.
	return utxoEntriesFound, nil
}

func DeleteUnmodifiedMappingsForUtxoWithTxn(txn *badger.Txn, utxoKey *UtxoKey) error {
	// Get the entry for the utxoKey from the db.
	utxoEntry := DbGetUtxoEntryForUtxoKeyWithTxn(txn, utxoKey)
	if utxoEntry == nil {
		// If an entry doesn't exist for this key then there is nothing in the
		// db to delete.
		return nil
	}

	// If the entry exists, delete the <UtxoKey -> UtxoEntry> mapping from the db.
	// It is assumed that the entry corresponding to a key has not been modified
	// and so is OK to delete
	if err := DeleteUtxoEntryForKeyWithTxn(txn, utxoKey); err != nil {
		return err
	}

	// Delete the <pubkey, utxoKey> -> <> mapping.
	if err := DeletePubKeyUtxoKeyMappingWithTxn(txn, utxoEntry.PublicKey, utxoKey); err != nil {
		return err
	}

	return nil
}

func PutMappingsForUtxoWithTxn(txn *badger.Txn, utxoKey *UtxoKey, utxoEntry *UtxoEntry) error {
	// Put the <utxoKey -> utxoEntry> mapping.
	if err := PutUtxoEntryForUtxoKeyWithTxn(txn, utxoKey, utxoEntry); err != nil {
		return nil
	}

	// Put the <pubkey, utxoKey> -> <> mapping.
	if err := PutPubKeyUtxoKeyWithTxn(txn, utxoEntry.PublicKey, utxoKey); err != nil {
		return err
	}

	return nil
}

func _DecodeUtxoOperations(data []byte) ([][]*UtxoOperation, error) {
	ret := [][]*UtxoOperation{}
	if err := gob.NewDecoder(bytes.NewReader(data)).Decode(&ret); err != nil {
		return nil, err
	}
	return ret, nil
}

func _EncodeUtxoOperations(utxoOp [][]*UtxoOperation) []byte {
	opBuf := bytes.NewBuffer([]byte{})
	gob.NewEncoder(opBuf).Encode(utxoOp)
	return opBuf.Bytes()
}

func _DbKeyForUtxoOps(blockHash *BlockHash) []byte {
	return append(append([]byte{}, _PrefixBlockHashToUtxoOperations...), blockHash[:]...)
}

func GetUtxoOperationsForBlockWithTxn(txn *badger.Txn, blockHash *BlockHash) ([][]*UtxoOperation, error) {
	var retOps [][]*UtxoOperation
	utxoOpsItem, err := txn.Get(_DbKeyForUtxoOps(blockHash))
	if err != nil {
		return nil, err
	}
	err = utxoOpsItem.Value(func(valBytes []byte) error {
		retOps, err = _DecodeUtxoOperations(valBytes)
		if err != nil {
			return err
		}

		return nil
	})

	if err != nil {
		return nil, err
	}

	return retOps, err
}

func GetUtxoOperationsForBlock(handle *badger.DB, blockHash *BlockHash) ([][]*UtxoOperation, error) {
	var ops [][]*UtxoOperation
	err := handle.View(func(txn *badger.Txn) error {
		var err error
		ops, err = GetUtxoOperationsForBlockWithTxn(txn, blockHash)
		return err
	})

	return ops, err
}

func PutUtxoOperationsForBlockWithTxn(txn *badger.Txn, blockHash *BlockHash, utxoOpsForBlock [][]*UtxoOperation) error {
	return txn.Set(_DbKeyForUtxoOps(blockHash), _EncodeUtxoOperations(utxoOpsForBlock))
}

func DeleteUtxoOperationsForBlockWithTxn(txn *badger.Txn, blockHash *BlockHash) error {
	return txn.Delete(_DbKeyForUtxoOps(blockHash))
}

func SerializeBlockNode(blockNode *BlockNode) ([]byte, error) {
	data := []byte{}

	// Hash
	if blockNode.Hash == nil {
		return nil, fmt.Errorf("SerializeBlockNode: Hash cannot be nil")
	}
	data = append(data, blockNode.Hash[:]...)

	// Height
	data = append(data, UintToBuf(uint64(blockNode.Height))...)

	// DifficultyTarget
	if blockNode.DifficultyTarget == nil {
		return nil, fmt.Errorf("SerializeBlockNode: DifficultyTarget cannot be nil")
	}
	data = append(data, blockNode.DifficultyTarget[:]...)

	// CumWork
	data = append(data, BigintToHash(blockNode.CumWork)[:]...)

	// Header
	serializedHeader, err := blockNode.Header.ToBytes(false)
	if err != nil {
		return nil, errors.Wrapf(err, "SerializeBlockNode: Problem serializing header")
	}
	data = append(data, IntToBuf(int64(len(serializedHeader)))...)
	data = append(data, serializedHeader...)

	// Status
	// It's assumed this field is one byte long.
	data = append(data, UintToBuf(uint64(blockNode.Status))...)

	return data, nil
}

func DeserializeBlockNode(data []byte) (*BlockNode, error) {
	blockNode := NewBlockNode(
		nil,          // Parent
		&BlockHash{}, // Hash
		0,            // Height
		&BlockHash{}, // DifficultyTarget
		nil,          // CumWork
		nil,          // Header
		StatusNone,   // Status

	)

	rr := bytes.NewReader(data)

	// Hash
	_, err := io.ReadFull(rr, blockNode.Hash[:])
	if err != nil {
		return nil, errors.Wrapf(err, "DeserializeBlockNode: Problem decoding Hash")
	}

	// Height
	height, err := ReadUvarint(rr)
	if err != nil {
		return nil, errors.Wrapf(err, "DeserializeBlockNode: Problem decoding Height")
	}
	blockNode.Height = uint32(height)

	// DifficultyTarget
	_, err = io.ReadFull(rr, blockNode.DifficultyTarget[:])
	if err != nil {
		return nil, errors.Wrapf(err, "DeserializeBlockNode: Problem decoding DifficultyTarget")
	}

	// CumWork
	tmp := BlockHash{}
	_, err = io.ReadFull(rr, tmp[:])
	if err != nil {
		return nil, errors.Wrapf(err, "DeserializeBlockNode: Problem decoding CumWork")
	}
	blockNode.CumWork = HashToBigint(&tmp)

	// Header
	payloadLen, err := ReadVarint(rr)
	if err != nil {
		return nil, errors.Wrapf(err, "DeserializeBlockNode: Problem decoding Header length")
	}
	headerBytes := make([]byte, payloadLen)
	_, err = io.ReadFull(rr, headerBytes[:])
	if err != nil {
		return nil, errors.Wrapf(err, "DeserializeBlockNode: Problem reading Header bytes")
	}
	blockNode.Header = NewMessage(MsgTypeHeader).(*MsgBitCloutHeader)
	err = blockNode.Header.FromBytes(headerBytes)
	if err != nil {
		return nil, errors.Wrapf(err, "DeserializeBlockNode: Problem parsing Header bytes")
	}

	// Status
	status, err := ReadUvarint(rr)
	if err != nil {
		return nil, errors.Wrapf(err, "DeserializeBlockNode: Problem decoding Status")
	}
	blockNode.Status = BlockStatus(uint32(status))

	return blockNode, nil
}

type ChainType uint8

const (
	ChainTypeBitCloutBlock = iota
	ChainTypeBitcoinHeader
)

func _prefixForChainType(chainType ChainType) []byte {
	var prefix []byte
	switch chainType {
	case ChainTypeBitCloutBlock:
		prefix = _KeyBestBitCloutBlockHash
	case ChainTypeBitcoinHeader:
		prefix = _KeyBestBitcoinHeaderHash
	default:
		glog.Errorf("_prefixForChainType: Unknown ChainType %d; this should never happen", chainType)
		return nil
	}

	return prefix
}

func DbGetBestHash(handle *badger.DB, chainType ChainType) *BlockHash {
	prefix := _prefixForChainType(chainType)
	if len(prefix) == 0 {
		glog.Errorf("DbGetBestHash: Problem getting prefix for ChainType: %d", chainType)
		return nil
	}
	return _getBlockHashForPrefix(handle, prefix)
}

func PutBestHashWithTxn(txn *badger.Txn, bh *BlockHash, chainType ChainType) error {
	prefix := _prefixForChainType(chainType)
	if len(prefix) == 0 {
		glog.Errorf("PutBestHashWithTxn: Problem getting prefix for ChainType: %d", chainType)
		return nil
	}
	return txn.Set(prefix, bh[:])
}

func PutBestHash(bh *BlockHash, handle *badger.DB, chainType ChainType) error {
	return handle.Update(func(txn *badger.Txn) error {
		return PutBestHashWithTxn(txn, bh, chainType)
	})
}

func BlockHashToBlockKey(blockHash *BlockHash) []byte {
	return append(append([]byte{}, _PrefixBlockHashToBlock...), blockHash[:]...)
}

func GetBlockWithTxn(txn *badger.Txn, blockHash *BlockHash) *MsgBitCloutBlock {
	hashKey := BlockHashToBlockKey(blockHash)
	var blockRet *MsgBitCloutBlock

	item, err := txn.Get(hashKey)
	if err != nil {
		return nil
	}

	err = item.Value(func(valBytes []byte) error {
		ret := NewMessage(MsgTypeBlock).(*MsgBitCloutBlock)
		if err := ret.FromBytes(valBytes); err != nil {
			return err
		}
		blockRet = ret

		return nil
	})
	if err != nil {
		return nil
	}

	return blockRet
}

func GetBlock(blockHash *BlockHash, handle *badger.DB) (*MsgBitCloutBlock, error) {
	hashKey := BlockHashToBlockKey(blockHash)
	var blockRet *MsgBitCloutBlock
	err := handle.View(func(txn *badger.Txn) error {
		item, err := txn.Get(hashKey)
		if err != nil {
			return err
		}

		err = item.Value(func(valBytes []byte) error {
			ret := NewMessage(MsgTypeBlock).(*MsgBitCloutBlock)
			if err := ret.FromBytes(valBytes); err != nil {
				return err
			}
			blockRet = ret

			return nil
		})

		if err != nil {
			return err
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return blockRet, nil
}

func PutBlockWithTxn(txn *badger.Txn, bitcloutBlock *MsgBitCloutBlock) error {
	if bitcloutBlock.Header == nil {
		return fmt.Errorf("PutBlockWithTxn: Header was nil in block %v", bitcloutBlock)
	}
	blockHash, err := bitcloutBlock.Header.Hash()
	if err != nil {
		return errors.Wrapf(err, "PutBlockWithTxn: Problem hashing header: ")
	}
	blockKey := BlockHashToBlockKey(blockHash)
	data, err := bitcloutBlock.ToBytes(false)
	if err != nil {
		return err
	}
	// First check to see if the block is already in the db.
	if _, err := txn.Get(blockKey); err == nil {
		// err == nil means the block already exists in the db so
		// no need to store it.
		return nil
	}
	// If the block is not in the db then set it.
	if err := txn.Set(blockKey, data); err != nil {
		return err
	}
	return nil
}

func PutBlock(bitcloutBlock *MsgBitCloutBlock, handle *badger.DB) error {
	err := handle.Update(func(txn *badger.Txn) error {
		return PutBlockWithTxn(txn, bitcloutBlock)
	})
	if err != nil {
		return err
	}

	return nil
}

func _heightHashToNodeIndexPrefix(bitcoinNodes bool) []byte {
	prefix := append([]byte{}, _PrefixHeightHashToNodeInfo...)
	if bitcoinNodes {
		prefix = append([]byte{}, _PrefixBitcoinHeightHashToNodeInfo...)
	}

	return prefix
}

func _heightHashToNodeIndexKey(height uint32, hash *BlockHash, bitcoinNodes bool) []byte {
	prefix := _heightHashToNodeIndexPrefix(bitcoinNodes)

	heightBytes := make([]byte, 4)
	binary.BigEndian.PutUint32(heightBytes[:], height)
	key := append(prefix, heightBytes[:]...)
	key = append(key, hash[:]...)

	return key
}

func GetHeightHashToNodeInfoWithTxn(
	txn *badger.Txn, height uint32, hash *BlockHash, bitcoinNodes bool) *BlockNode {

	key := _heightHashToNodeIndexKey(height, hash, bitcoinNodes)
	nodeValue, err := txn.Get(key)
	if err != nil {
		return nil
	}
	var blockNode *BlockNode
	nodeValue.Value(func(nodeBytes []byte) error {
		blockNode, err = DeserializeBlockNode(nodeBytes)
		if err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return nil
	}
	return blockNode
}

func GetHeightHashToNodeInfo(
	handle *badger.DB, height uint32, hash *BlockHash, bitcoinNodes bool) *BlockNode {

	var blockNode *BlockNode
	handle.View(func(txn *badger.Txn) error {
		blockNode = GetHeightHashToNodeInfoWithTxn(txn, height, hash, bitcoinNodes)
		return nil
	})
	return blockNode
}

func PutHeightHashToNodeInfoWithTxn(txn *badger.Txn, node *BlockNode, bitcoinNodes bool) error {

	key := _heightHashToNodeIndexKey(node.Height, node.Hash, bitcoinNodes)
	serializedNode, err := SerializeBlockNode(node)
	if err != nil {
		return errors.Wrapf(err, "PutHeightHashToNodeInfoWithTxn: Problem serializing node")
	}

	if err := txn.Set(key, serializedNode); err != nil {
		return err
	}
	return nil
}

func PutHeightHashToNodeInfo(node *BlockNode, handle *badger.DB, bitcoinNodes bool) error {
	err := handle.Update(func(txn *badger.Txn) error {
		return PutHeightHashToNodeInfoWithTxn(txn, node, bitcoinNodes)
	})

	if err != nil {
		return err
	}

	return nil
}

func DbDeleteHeightHashToNodeInfoWithTxn(
	node *BlockNode, txn *badger.Txn, bitcoinNodes bool) error {

	return txn.Delete(_heightHashToNodeIndexKey(node.Height, node.Hash, bitcoinNodes))
}

func DbBulkDeleteHeightHashToNodeInfo(
	nodes []*BlockNode, handle *badger.DB, bitcoinNodes bool) error {

	err := handle.Update(func(txn *badger.Txn) error {
		for _, nn := range nodes {
			if err := DbDeleteHeightHashToNodeInfoWithTxn(nn, txn, bitcoinNodes); err != nil {
				return err
			}
		}
		return nil
	})

	if err != nil {
		return err
	}

	return nil
}

// InitDbWithGenesisBlock initializes the database to contain only the genesis
// block.
func InitDbWithBitCloutGenesisBlock(params *BitCloutParams, handle *badger.DB) error {
	// Construct a node for the genesis block. Its height is zero and it has
	// no parents. Its difficulty should be set to the initial
	// difficulty specified in the parameters and it should be assumed to be
	// valid and stored by the end of this function.
	genesisBlock := params.GenesisBlock
	diffTarget := NewBlockHash(params.MinDifficultyTargetHex)
	blockHash := NewBlockHash(params.GenesisBlockHashHex)
	genesisNode := NewBlockNode(
		nil, // Parent
		blockHash,
		0, // Height
		diffTarget,
		BytesToBigint(ExpectedWorkForBlockHash(diffTarget)[:]), // CumWork
		genesisBlock.Header, // Header
		StatusHeaderValidated|StatusBlockProcessed|StatusBlockStored|StatusBlockValidated, // Status
	)

	// Set the fields in the db to reflect the current state of our chain.
	//
	// Set the best hash to the genesis block in the db since its the only node
	// we're currently aware of. Set it for both the header chain and the block
	// chain.
	if err := PutBestHash(blockHash, handle, ChainTypeBitCloutBlock); err != nil {
		return errors.Wrapf(err, "InitDbWithGenesisBlock: Problem putting genesis block hash into db for block chain")
	}
	// Add the genesis block to the (hash -> block) index.
	if err := PutBlock(genesisBlock, handle); err != nil {
		return errors.Wrapf(err, "InitDbWithGenesisBlock: Problem putting genesis block into db")
	}
	// Add the genesis block to the (height, hash -> node info) index in the db.
	if err := PutHeightHashToNodeInfo(genesisNode, handle, false /*bitcoinNodes*/); err != nil {
		return errors.Wrapf(err, "InitDbWithGenesisBlock: Problem putting (height, hash -> node) in db")
	}
	if err := DbPutNanosPurchased(handle, params.BitCloutNanosPurchasedAtGenesis); err != nil {
		return errors.Wrapf(err, "InitDbWithGenesisBlock: Problem putting genesis block hash into db for block chain")
	}
	if err := DbPutGlobalParamsEntry(handle, InitialGlobalParamsEntry); err != nil {
		return errors.Wrapf(err, "InitDbWithGenesisBlock: Problem putting GlobalParamsEntry into db for block chain")
	}

	// We apply seed transactions here. This step is useful for setting
	// up the blockchain with a particular set of transactions, e.g. when
	// hard forking the chain.
	//
	// TODO: Right now there's an issue where if we hit an errur during this
	// step of the initialization, the next time we run the program it will
	// think things are initialized because we set the best block hash at the
	// top. We should fix this at some point so that an error in this step
	// wipes out the best hash.
	utxoView, err := NewUtxoView(handle, params, nil)
	if err != nil {
		return fmt.Errorf(
			"InitDbWithBitCloutGenesisBlock: Error initializing UtxoView")
	}

	// Add the seed balances to the view.
	for index, txOutput := range params.SeedBalances {
		outputKey := UtxoKey{
			TxID:  BlockHash{},
			Index: uint32(index),
		}
		utxoEntry := UtxoEntry{
			AmountNanos: txOutput.AmountNanos,
			PublicKey:   txOutput.PublicKey,
			BlockHeight: 0,
			// Just make this a normal transaction so that we don't have to wait for
			// the block reward maturity.
			UtxoType: UtxoTypeOutput,
			UtxoKey:  &outputKey,
		}

		_, err := utxoView._addUtxo(&utxoEntry)
		if err != nil {
			return fmt.Errorf("InitDbWithBitCloutGenesisBlock: Error adding "+
				"seed balance at index %v ; output: %v: %v", index, txOutput, err)
		}
	}

	// Add the seed txns to the view
	for txnIndex, txnHex := range params.SeedTxns {
		txnBytes, err := hex.DecodeString(txnHex)
		if err != nil {
			return fmt.Errorf(
				"InitDbWithBitCloutGenesisBlock: Error decoding seed "+
					"txn HEX: %v, txn index: %v, txn hex: %v",
				err, txnIndex, txnHex)
		}
		txn := &MsgBitCloutTxn{}
		if err := txn.FromBytes(txnBytes); err != nil {
			return fmt.Errorf(
				"InitDbWithBitCloutGenesisBlock: Error decoding seed "+
					"txn BYTES: %v, txn index: %v, txn hex: %v",
				err, txnIndex, txnHex)
		}
		// Important: ignoreUtxos makes it so that the inputs/outputs aren't
		// processed, which is important.
		// Set txnSizeBytes to 0 here as the minimum network fee is 0 at genesis block, so there is no need to serialize
		// these transactions to check if they meet the minimum network fee requirement.
		_, _, _, _, err = utxoView.ConnectTransaction(
			txn, txn.Hash(), 0, 0 /*blockHeight*/, false /*verifySignatures*/, true /*ignoreUtxos*/)
		if err != nil {
			return fmt.Errorf(
				"InitDbWithBitCloutGenesisBlock: Error connecting transaction: %v, "+
					"txn index: %v, txn hex: %v",
				err, txnIndex, txnHex)
		}
	}
	// Flush all the data in the view.
	err = utxoView.FlushToDb()
	if err != nil {
		return fmt.Errorf(
			"InitDbWithBitCloutGenesisBlock: Error flushing seed txns to DB: %v", err)
	}

	return nil
}

func GetBlockIndex(handle *badger.DB, bitcoinNodes bool) (map[BlockHash]*BlockNode, error) {
	blockIndex := make(map[BlockHash]*BlockNode)

	prefix := _heightHashToNodeIndexPrefix(bitcoinNodes)

	err := handle.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		nodeIterator := txn.NewIterator(opts)
		defer nodeIterator.Close()
		for nodeIterator.Seek(prefix); nodeIterator.ValidForPrefix(prefix); nodeIterator.Next() {
			var blockNode *BlockNode

			// Don't bother checking the key. We assume that the key lines up
			// with what we've stored in the value in terms of (height, block hash).
			item := nodeIterator.Item()
			err := item.Value(func(blockNodeBytes []byte) error {
				// Deserialize the block node.
				var err error
				// TODO: There is room for optimization here by pre-allocating a
				// contiguous list of block nodes and then populating that list
				// rather than having each blockNode be a stand-alone allocation.
				blockNode, err = DeserializeBlockNode(blockNodeBytes)
				if err != nil {
					return err
				}
				return nil
			})
			if err != nil {
				return err
			}

			// If we got hear it means we read a blockNode successfully. Store it
			// into our node index.
			blockIndex[*blockNode.Hash] = blockNode

			// Find the parent of this block, which should already have been read
			// in and connect it. Skip the genesis block, which has height 0. Also
			// skip the block if its PrevBlockHash is empty, which will be true for
			// the BitcoinStartBlockNode.
			//
			// TODO: There is room for optimization here by keeping a reference to
			// the last node we've iterated over and checking if that node is the
			// parent. Doing this would avoid an expensive hashmap check to get
			// the parent by its block hash.
			if blockNode.Height == 0 || (*blockNode.Header.PrevBlockHash == BlockHash{}) {
				continue
			}
			if parent, ok := blockIndex[*blockNode.Header.PrevBlockHash]; ok {
				// We found the parent node so connect it.
				blockNode.Parent = parent
			} else {
				// In this case we didn't find the parent so error. There shouldn't
				// be any unconnectedTxns in our block index.
				return fmt.Errorf("GetBlockIndex: Could not find parent for blockNode: %+v", blockNode)
			}
		}
		return nil
	})
	if err != nil {
		return nil, errors.Wrapf(err, "GetBlockIndex: Problem reading block index from db")
	}

	return blockIndex, nil
}

func GetBestChain(tipNode *BlockNode, blockIndex map[BlockHash]*BlockNode) ([]*BlockNode, error) {
	reversedBestChain := []*BlockNode{}
	for tipNode != nil {
		if (tipNode.Status&StatusBlockValidated) == 0 &&
			(tipNode.Status&StatusBitcoinHeaderValidated) == 0 {

			return nil, fmt.Errorf("GetBestChain: Invalid node found in main chain: %+v", tipNode)
		}

		reversedBestChain = append(reversedBestChain, tipNode)
		tipNode = tipNode.Parent
	}

	bestChain := make([]*BlockNode, len(reversedBestChain))
	for ii := 0; ii < len(reversedBestChain); ii++ {
		bestChain[ii] = reversedBestChain[len(reversedBestChain)-1-ii]
	}

	return bestChain, nil
}

// RandomBytes returns a []byte with random values.
func RandomBytes(numBytes int32) []byte {
	randomBytes := make([]byte, numBytes)
	_, err := rand.Read(randomBytes)
	if err != nil {
		glog.Errorf("Problem reading random bytes: %v", err)
	}
	return randomBytes
}

// RandomBytesHex returns a hex string representing numBytes of
// entropy.
func RandomBytesHex(numBytes int32) string {
	return hex.EncodeToString(RandomBytes(numBytes))
}

// RandInt64 returns a random 64-bit int.
func RandInt64(max int64) int64 {
	val, err := rand.Int(rand.Reader, big.NewInt(math.MaxInt64))
	if err != nil {
		glog.Errorf("Problem generating random int64: %v", err)
	}
	return val.Int64()
}

// RandInt32 returns a random 32-bit int.
func RandInt32(max int32) int32 {
	val, err := rand.Int(rand.Reader, big.NewInt(math.MaxInt32))
	if err != nil {
		glog.Errorf("Problem generating random int32: %v", err)
	}
	if val.Int64() > math.MaxInt32 {
		glog.Errorf("Generated a random number out of range: %d (max: %d)", val.Int64(), math.MaxInt32)
	}
	// This cast is OK since we initialized the number to be
	// < MaxInt32 above.
	return int32(val.Int64())
}

// PPrintJSON prints a JSON object but pretty.
func PPrintJSON(xx interface{}) {
	yy, _ := json.MarshalIndent(xx, "", "  ")
	log.Println(string(yy))
}

func BlocksPerDuration(duration time.Duration, timeBetweenBlocks time.Duration) uint32 {
	return uint32(int64(duration) / int64(timeBetweenBlocks))
}

func PkToString(pk []byte, params *BitCloutParams) string {
	return Base58CheckEncode(pk, false, params)
}

func PrivToString(priv []byte, params *BitCloutParams) string {
	return Base58CheckEncode(priv, true, params)
}

func PkToStringMainnet(pk []byte) string {
	return Base58CheckEncode(pk, false, &BitCloutMainnetParams)
}

func PkToStringBoth(pk []byte) string {
	return PkToStringMainnet(pk) + ":" + PkToStringTestnet(pk)
}

func PkToStringTestnet(pk []byte) string {
	return Base58CheckEncode(pk, false, &BitCloutTestnetParams)
}

func DbGetTxindexTip(handle *badger.DB) *BlockHash {
	return _getBlockHashForPrefix(handle, _KeyTransactionIndexTip)
}

func DbPutTxindexTipWithTxn(dbTxn *badger.Txn, tipHash *BlockHash) error {
	return dbTxn.Set(_KeyTransactionIndexTip, tipHash[:])
}

func DbPutTxindexTip(handle *badger.DB, tipHash *BlockHash) error {
	return handle.Update(func(txn *badger.Txn) error {
		return DbPutTxindexTipWithTxn(txn, tipHash)
	})
}

func _DbTxindexPublicKeyNextIndexPrefix(publicKey []byte) []byte {
	return append(append([]byte{}, _PrefixPublicKeyToNextIndex...), publicKey...)
}

func DbTxindexPublicKeyPrefix(publicKey []byte) []byte {
	return append(append([]byte{}, _PrefixPublicKeyIndexToTransactionIDs...), publicKey...)
}

func DbTxindexPublicKeyIndexToTxnKey(publicKey []byte, index uint32) []byte {
	prefix := DbTxindexPublicKeyPrefix(publicKey)
	return append(prefix, _EncodeUint32(index)...)
}

func DbGetTxindexTxnsForPublicKeyWithTxn(dbTxn *badger.Txn, publicKey []byte) []*BlockHash {
	txIDs := []*BlockHash{}
	_, valsFound, err := _enumerateKeysForPrefixWithTxn(dbTxn, DbTxindexPublicKeyPrefix(publicKey))
	if err != nil {
		return txIDs
	}
	for _, txIDBytes := range valsFound {
		blockHash := &BlockHash{}
		copy(blockHash[:], txIDBytes[:])
		txIDs = append(txIDs, blockHash)
	}

	return txIDs
}

func DbGetTxindexTxnsForPublicKey(handle *badger.DB, publicKey []byte) []*BlockHash {
	txIDs := []*BlockHash{}
	handle.Update(func(dbTxn *badger.Txn) error {
		txIDs = DbGetTxindexTxnsForPublicKeyWithTxn(dbTxn, publicKey)
		return nil
	})
	return txIDs
}

func _DbGetTxindexNextIndexForPublicKeBySeekWithTxn(dbTxn *badger.Txn, publicKey []byte) uint64 {
	dbPrefixx := DbTxindexPublicKeyPrefix(publicKey)

	opts := badger.DefaultIteratorOptions

	opts.PrefetchValues = false

	// Go in reverse order.
	opts.Reverse = true

	it := dbTxn.NewIterator(opts)
	defer it.Close()
	// Since we iterate backwards, the prefix must be bigger than all possible
	// counts that could actually exist. We use four bytes since the index is
	// encoded as a 32-bit big-endian byte slice, which will be four bytes long.
	maxBigEndianUint32Bytes := []byte{0xFF, 0xFF, 0xFF, 0xFF}
	prefix := append([]byte{}, dbPrefixx...)
	prefix = append(prefix, maxBigEndianUint32Bytes...)
	for it.Seek(prefix); it.ValidForPrefix(dbPrefixx); it.Next() {
		countKey := it.Item().Key()

		// Strip the prefix off the key and check its length. If it contains
		// a big-endian uint32 then it should be at least four bytes.
		countKey = countKey[len(dbPrefixx):]
		if len(countKey) < len(maxBigEndianUint32Bytes) {
			glog.Errorf("DbGetTxindexNextIndexForPublicKey: Invalid public key "+
				"index key length %d should be at least %d",
				len(countKey), len(maxBigEndianUint32Bytes))
			return 0
		}

		countVal := DecodeUint32(countKey[:len(maxBigEndianUint32Bytes)])
		return uint64(countVal + 1)
	}
	// If we get here it means we didn't find anything in the db so return zero.
	return 0
}

func DbGetTxindexNextIndexForPublicKey(handle *badger.DB, publicKey []byte) *uint64 {
	var nextIndex *uint64
	handle.View(func(txn *badger.Txn) error {
		nextIndex = _DbGetTxindexNextIndexForPublicKeyWithTxn(txn, publicKey)
		return nil
	})
	return nextIndex
}

func _DbGetTxindexNextIndexForPublicKeyWithTxn(txn *badger.Txn, publicKey []byte) *uint64 {
	key := _DbTxindexPublicKeyNextIndexPrefix(publicKey)
	valItem, err := txn.Get(key)
	if err != nil {
		// If we haven't seen this public key yet, we won't have a next index for this key yet, so return 0.
		if errors.Is(err, badger.ErrKeyNotFound) {
			nextIndexVal := _DbGetTxindexNextIndexForPublicKeBySeekWithTxn(txn, publicKey)
			return &nextIndexVal
		} else {
			return nil
		}
	}
	valBytes, err := valItem.ValueCopy(nil)
	if err != nil {
		return nil
	}
	nextIndexVal, bytesRead := Uvarint(valBytes)
	if bytesRead <= 0 {
		return nil
	}
	return &nextIndexVal

}

func DbPutTxindexNextIndexForPublicKeyWithTxn(txn *badger.Txn, publicKey []byte, nextIndex uint64) error {
	key := _DbTxindexPublicKeyNextIndexPrefix(publicKey)
	valBuf := UintToBuf(nextIndex)

	return txn.Set(key, valBuf)
}

func DbDeleteTxindexNextIndexForPublicKeyWithTxn(txn *badger.Txn, publicKey []byte) error {
	key := _DbTxindexPublicKeyNextIndexPrefix(publicKey)
	return txn.Delete(key)
}

func DbPutTxindexPublicKeyToTxnMappingSingleWithTxn(
	dbTxn *badger.Txn, publicKey []byte, txID *BlockHash) error {

	nextIndex := _DbGetTxindexNextIndexForPublicKeyWithTxn(dbTxn, publicKey)
	if nextIndex == nil {
		return fmt.Errorf("Error getting next index")
	}
	key := DbTxindexPublicKeyIndexToTxnKey(publicKey, uint32(*nextIndex))
	err := DbPutTxindexNextIndexForPublicKeyWithTxn(dbTxn, publicKey, uint64(*nextIndex+1))
	if err != nil {
		return err
	}
	return dbTxn.Set(key, txID[:])
}

func DbDeleteTxindexPublicKeyToTxnMappingSingleWithTxn(
	dbTxn *badger.Txn, publicKey []byte, txID *BlockHash) error {

	// Get all the mappings corresponding to the public key passed in.
	// TODO: This is inefficient but reorgs are rare so whatever.
	txIDsInDB := DbGetTxindexTxnsForPublicKeyWithTxn(dbTxn, publicKey)
	numMappingsInDB := len(txIDsInDB)

	// Loop over the list of txIDs and delete the one
	// corresponding to the passed-in transaction. Note we can assume that
	// only one occurrence exists in the list.
	// TODO: Looping backwards would be more efficient.
	for ii, singleTxID := range txIDsInDB {
		if *singleTxID == *txID {
			// If we get here it means the transaction we need to delete is at
			// this index.
			txIDsInDB = append(txIDsInDB[:ii], txIDsInDB[ii+1:]...)
			break
		}
	}

	// Delete all the mappings from the db.
	for pkIndex := 0; pkIndex < numMappingsInDB; pkIndex++ {
		key := DbTxindexPublicKeyIndexToTxnKey(publicKey, uint32(pkIndex))
		if err := dbTxn.Delete(key); err != nil {
			return err
		}
	}

	// Delete the next index for this public key
	err := DbDeleteTxindexNextIndexForPublicKeyWithTxn(dbTxn, publicKey)
	if err != nil {
		return err
	}

	// Re-add all the mappings to the db except the one we just deleted.
	for _, singleTxID := range txIDsInDB {
		if err := DbPutTxindexPublicKeyToTxnMappingSingleWithTxn(dbTxn, publicKey, singleTxID); err != nil {
			return err
		}
	}

	// At this point the db should contain all transactions except the one
	// that was deleted.
	return nil
}

func DbTxindexTxIDKey(txID *BlockHash) []byte {
	return append(append([]byte{}, _PrefixTransactionIDToMetadata...), txID[:]...)
}

type AffectedPublicKey struct {
	PublicKeyBase58Check string
	// Metadata about how this public key was affected by the transaction.
	Metadata string
}

type BasicTransferTxindexMetadata struct {
	TotalInputNanos  uint64
	TotalOutputNanos uint64
	FeeNanos         uint64
	UtxoOpsDump      string
}
type BitcoinExchangeTxindexMetadata struct {
	BitcoinSpendAddress string
	// BitCloutOutputPubKeyBase58Check = TransactorPublicKeyBase58Check
	SatoshisBurned uint64
	// NanosCreated = 0 OR TotalOutputNanos+FeeNanos
	NanosCreated uint64
	// TotalNanosPurchasedBefore = TotalNanosPurchasedAfter - NanosCreated
	TotalNanosPurchasedBefore uint64
	TotalNanosPurchasedAfter  uint64
	BitcoinTxnHash            string
}
type CreatorCoinTxindexMetadata struct {
	OperationType string
	// TransactorPublicKeyBase58Check = TransactorPublicKeyBase58Check
	// CreatorPublicKeyBase58Check in AffectedPublicKeys

	// Differs depending on OperationType.
	BitCloutToSellNanos    uint64
	CreatorCoinToSellNanos uint64
	BitCloutToAddNanos     uint64
}

type CreatorCoinTransferTxindexMetadata struct {
	CreatorUsername            string
	CreatorCoinToTransferNanos uint64
	DiamondLevel               int64
	PostHashHex                string
}

type UpdateProfileTxindexMetadata struct {
	ProfilePublicKeyBase58Check string

	NewUsername    string
	NewDescription string
	NewProfilePic  string

	NewCreatorBasisPoints uint64

	NewStakeMultipleBasisPoints uint64

	IsHidden bool
}
type SubmitPostTxindexMetadata struct {
	PostHashBeingModifiedHex string
	// PosterPublicKeyBase58Check = TransactorPublicKeyBase58Check

	// If this is a reply to an existing post, then the ParentPostHashHex
	ParentPostHashHex string
	// ParentPosterPublicKeyBase58Check in AffectedPublicKeys

	// The profiles that are mentioned are in the AffectedPublicKeys
	// MentionedPublicKeyBase58Check in AffectedPublicKeys
}
type LikeTxindexMetadata struct {
	// LikerPublicKeyBase58Check = TransactorPublicKeyBase58Check
	IsUnlike bool

	PostHashHex string
	// PosterPublicKeyBase58Check in AffectedPublicKeys
}
type FollowTxindexMetadata struct {
	// FollowerPublicKeyBase58Check = TransactorPublicKeyBase58Check
	// FollowedPublicKeyBase58Check in AffectedPublicKeys

	IsUnfollow bool
}
type PrivateMessageTxindexMetadata struct {
	// SenderPublicKeyBase58Check = TransactorPublicKeyBase58Check
	// RecipientPublicKeyBase58Check in AffectedPublicKeys

	TimestampNanos uint64
}
type SwapIdentityTxindexMetadata struct {
	// ParamUpdater = TransactorPublicKeyBase58Check

	FromPublicKeyBase58Check string

	ToPublicKeyBase58Check string
}

type TransactionMetadata struct {
	BlockHashHex    string
	TxnIndexInBlock uint64
	TxnType         string
	// All transactions have a public key who executed the transaction and some
	// public keys that are affected by the transaction. Notifications are created
	// for the affected public keys. _getPublicKeysForTxn uses this to set entries in the
	// database.
	TransactorPublicKeyBase58Check string
	AffectedPublicKeys             []*AffectedPublicKey

	// We store these outputs so we don't have to load the full transaction from disk
	// when looking up output amounts
	TxnOutputs []*BitCloutOutput

	BasicTransferTxindexMetadata       *BasicTransferTxindexMetadata
	BitcoinExchangeTxindexMetadata     *BitcoinExchangeTxindexMetadata
	CreatorCoinTxindexMetadata         *CreatorCoinTxindexMetadata
	CreatorCoinTransferTxindexMetadata *CreatorCoinTransferTxindexMetadata
	UpdateProfileTxindexMetadata       *UpdateProfileTxindexMetadata
	SubmitPostTxindexMetadata          *SubmitPostTxindexMetadata
	LikeTxindexMetadata                *LikeTxindexMetadata
	FollowTxindexMetadata              *FollowTxindexMetadata
	PrivateMessageTxindexMetadata      *PrivateMessageTxindexMetadata
	SwapIdentityTxindexMetadata        *SwapIdentityTxindexMetadata
}

func DbGetTxindexTransactionRefByTxIDWithTxn(txn *badger.Txn, txID *BlockHash) *TransactionMetadata {
	key := DbTxindexTxIDKey(txID)
	valObj := TransactionMetadata{}

	valItem, err := txn.Get(key)
	if err != nil {
		return nil
	}
	valBytes, err := valItem.ValueCopy(nil)
	if err != nil {
		return nil
	}
	if err := gob.NewDecoder(bytes.NewReader(valBytes)).Decode(&valObj); err != nil {
		return nil
	}
	return &valObj
}

func DbGetTxindexTransactionRefByTxID(handle *badger.DB, txID *BlockHash) *TransactionMetadata {
	var valObj *TransactionMetadata
	handle.View(func(txn *badger.Txn) error {
		valObj = DbGetTxindexTransactionRefByTxIDWithTxn(txn, txID)
		return nil
	})
	return valObj
}
func DbPutTxindexTransactionWithTxn(
	txn *badger.Txn, txID *BlockHash, txnMeta *TransactionMetadata) error {

	key := append(append([]byte{}, _PrefixTransactionIDToMetadata...), txID[:]...)
	valBuf := bytes.NewBuffer([]byte{})
	gob.NewEncoder(valBuf).Encode(txnMeta)

	return txn.Set(key, valBuf.Bytes())
}

func DbPutTxindexTransaction(
	handle *badger.DB, txID *BlockHash, txnMeta *TransactionMetadata) error {

	return handle.Update(func(txn *badger.Txn) error {
		return DbPutTxindexTransactionWithTxn(txn, txID, txnMeta)
	})
}

func _getPublicKeysForTxn(
	txn *MsgBitCloutTxn, txnMeta *TransactionMetadata, params *BitCloutParams) map[PkMapKey]bool {

	// Collect the public keys in the transaction.
	publicKeys := make(map[PkMapKey]bool)

	// TODO: For AddStake transactions, we don't have a way of getting the implicit
	// outputs. This means that if you get paid from someone else staking to a post
	// after you, the output won't be explicitly included in the transaction, and so
	// it won't be added to our index. We should fix this at some point. I think the
	// "right way" to fix this problem is to index UTXOs rather than transactions (or
	// in addition to them).
	// TODO(updated): We can fix this by populating AffectedPublicKeys

	// Add the TransactorPublicKey
	{
		res, _, err := Base58CheckDecode(txnMeta.TransactorPublicKeyBase58Check)
		if err != nil {
			glog.Errorf("_getPublicKeysForTxn: Error decoding "+
				"TransactorPublicKeyBase58Check: %v %v",
				txnMeta.TransactorPublicKeyBase58Check, err)
		} else {
			publicKeys[MakePkMapKey(res)] = true
		}
	}

	// Add each AffectedPublicKey
	for _, affectedPk := range txnMeta.AffectedPublicKeys {
		res, _, err := Base58CheckDecode(affectedPk.PublicKeyBase58Check)
		if err != nil {
			glog.Errorf("_getPublicKeysForTxn: Error decoding AffectedPublicKey: %v %v %v",
				affectedPk.PublicKeyBase58Check, affectedPk.Metadata, err)
		} else {
			publicKeys[MakePkMapKey(res)] = true
		}
	}

	return publicKeys
}

func DbPutTxindexTransactionMappingsWithTxn(
	dbTx *badger.Txn, txn *MsgBitCloutTxn, params *BitCloutParams, txnMeta *TransactionMetadata) error {

	txID := txn.Hash()

	if err := DbPutTxindexTransactionWithTxn(dbTx, txID, txnMeta); err != nil {
		return fmt.Errorf("Problem adding txn to txindex transaction index: %v", err)
	}

	// Get the public keys involved with this transaction.
	publicKeys := _getPublicKeysForTxn(txn, txnMeta, params)

	// For each public key found, add the txID from its list.
	for pkFound := range publicKeys {
		// Simply add a new entry for each of the public keys found.
		if err := DbPutTxindexPublicKeyToTxnMappingSingleWithTxn(dbTx, pkFound[:], txID); err != nil {
			return err
		}
	}

	// If we get here, it means everything went smoothly.
	return nil
}

func DbPutTxindexTransactionMappings(
	handle *badger.DB, bitcloutTxn *MsgBitCloutTxn, params *BitCloutParams, txnMeta *TransactionMetadata) error {

	return handle.Update(func(dbTx *badger.Txn) error {
		return DbPutTxindexTransactionMappingsWithTxn(
			dbTx, bitcloutTxn, params, txnMeta)
	})
}

func DbDeleteTxindexTransactionMappingsWithTxn(
	dbTxn *badger.Txn, txn *MsgBitCloutTxn, params *BitCloutParams) error {

	txID := txn.Hash()

	// If the txnMeta isn't in the db then that's an error.
	txnMeta := DbGetTxindexTransactionRefByTxIDWithTxn(dbTxn, txID)
	if txnMeta == nil {
		return fmt.Errorf("DbDeleteTxindexTransactionMappingsWithTxn: Missing txnMeta for txID %v", txID)
	}

	// Get the public keys involved with this transaction.
	publicKeys := _getPublicKeysForTxn(txn, txnMeta, params)

	// For each public key found, delete the txID mapping from the db.
	for pkFound := range publicKeys {
		if err := DbDeleteTxindexPublicKeyToTxnMappingSingleWithTxn(dbTxn, pkFound[:], txID); err != nil {
			return err
		}
	}

	// Delete the metadata
	transactionIndexKey := DbTxindexTxIDKey(txID)
	if err := dbTxn.Delete(transactionIndexKey); err != nil {
		return fmt.Errorf("Problem deleting transaction index key: %v", err)
	}

	// If we get here, it means everything went smoothly.
	return nil
}

func DbDeleteTxindexTransactionMappings(
	handle *badger.DB, txn *MsgBitCloutTxn, params *BitCloutParams) error {

	return handle.Update(func(dbTx *badger.Txn) error {
		return DbDeleteTxindexTransactionMappingsWithTxn(dbTx, txn, params)
	})
}

// DbGetTxindexFullTransactionByTxID
// TODO: This makes lookups inefficient when blocks are large. Shouldn't be a
// problem for a while, but keep an eye on it.
func DbGetTxindexFullTransactionByTxID(
	txindexDBHandle *badger.DB, blockchainDBHandle *badger.DB, txID *BlockHash) (
	_txn *MsgBitCloutTxn, _txnMeta *TransactionMetadata) {

	var txnFound *MsgBitCloutTxn
	var txnMeta *TransactionMetadata
	err := txindexDBHandle.View(func(dbTxn *badger.Txn) error {
		txnMeta = DbGetTxindexTransactionRefByTxIDWithTxn(dbTxn, txID)
		if txnMeta == nil {
			return fmt.Errorf("DbGetTxindexFullTransactionByTxID: Transaction not found")
		}
		blockHashBytes, err := hex.DecodeString(txnMeta.BlockHashHex)
		if err != nil {
			return fmt.Errorf("DbGetTxindexFullTransactionByTxID: Error parsing block "+
				"hash hex: %v %v", txnMeta.BlockHashHex, err)
		}
		blockHash := &BlockHash{}
		copy(blockHash[:], blockHashBytes)
		blockFound, err := GetBlock(blockHash, blockchainDBHandle)
		if blockFound == nil || err != nil {
			return fmt.Errorf("DbGetTxindexFullTransactionByTxID: Block corresponding to txn not found")
		}

		txnFound = blockFound.Txns[txnMeta.TxnIndexInBlock]
		return nil
	})
	if err != nil {
		return nil, nil
	}

	return txnFound, txnMeta
}

// =======================================================================================
// BitClout app code start
// =======================================================================================

func _dbKeyForPostEntryHash(postHash *BlockHash) []byte {
	// Make a copy to avoid multiple calls to this function re-using the same slice.
	prefixCopy := append([]byte{}, _PrefixPostHashToPostEntry...)
	key := append(prefixCopy, postHash[:]...)
	return key
}
func _dbKeyForPublicKeyPostHash(publicKey []byte, postHash *BlockHash) []byte {
	// Make a copy to avoid multiple calls to this function re-using the same slice.
	key := append([]byte{}, _PrefixPosterPublicKeyPostHash...)
	key = append(key, publicKey...)
	key = append(key, postHash[:]...)
	return key
}
func _dbKeyForPosterPublicKeyTimestampPostHash(publicKey []byte, timestampNanos uint64, postHash *BlockHash) []byte {
	// Make a copy to avoid multiple calls to this function re-using the same slice.
	key := append([]byte{}, _PrefixPosterPublicKeyTimestampPostHash...)
	key = append(key, publicKey...)
	key = append(key, EncodeUint64(timestampNanos)...)
	key = append(key, postHash[:]...)
	return key
}
func _dbKeyForTstampPostHash(tstampNanos uint64, postHash *BlockHash) []byte {
	// Make a copy to avoid multiple calls to this function re-using the same slice.
	key := append([]byte{}, _PrefixTstampNanosPostHash...)
	key = append(key, EncodeUint64(tstampNanos)...)
	key = append(key, postHash[:]...)
	return key
}
func _dbKeyForCreatorBpsPostHash(creatorBps uint64, postHash *BlockHash) []byte {
	key := append([]byte{}, _PrefixCreatorBpsPostHash...)
	key = append(key, EncodeUint64(creatorBps)...)
	key = append(key, postHash[:]...)
	return key
}
func _dbKeyForStakeMultipleBpsPostHash(stakeMultipleBps uint64, postHash *BlockHash) []byte {
	key := append([]byte{}, _PrefixMultipleBpsPostHash...)
	key = append(key, EncodeUint64(stakeMultipleBps)...)
	key = append(key, postHash[:]...)
	return key
}
func _dbKeyForCommentParentStakeIDToPostHash(
	stakeID []byte, tstampNanos uint64, postHash *BlockHash) []byte {
	key := append([]byte{}, _PrefixCommentParentStakeIDToPostHash...)
	key = append(key, stakeID[:]...)
	key = append(key, EncodeUint64(tstampNanos)...)
	key = append(key, postHash[:]...)
	return key
}

func DBGetPostEntryByPostHashWithTxn(
	txn *badger.Txn, postHash *BlockHash) *PostEntry {

	key := _dbKeyForPostEntryHash(postHash)
	postEntryObj := &PostEntry{}
	postEntryItem, err := txn.Get(key)
	if err != nil {
		return nil
	}
	err = postEntryItem.Value(func(valBytes []byte) error {
		return gob.NewDecoder(bytes.NewReader(valBytes)).Decode(postEntryObj)
	})
	if err != nil {
		glog.Errorf("DBGetPostEntryByPostHashWithTxn: Problem reading "+
			"PostEntry for postHash %v", postHash)
		return nil
	}
	return postEntryObj
}

func DBGetPostEntryByPostHash(db *badger.DB, postHash *BlockHash) *PostEntry {
	var ret *PostEntry
	db.View(func(txn *badger.Txn) error {
		ret = DBGetPostEntryByPostHashWithTxn(txn, postHash)
		return nil
	})
	return ret
}

func _dbGetStakeIDPostDBKey(postHash *BlockHash, totalAmountStakedNanos uint64) []byte {
	// <prefix, StakeIDType | AmountNanos uint64 | PostHash BlockHash> -> <>
	key := append(_PrefixStakeIDTypeAmountStakeIDIndex, []byte{byte(StakeIDTypePost)}...)
	key = append(key, EncodeUint64(totalAmountStakedNanos)...)
	key = append(key, postHash[:]...)
	return key
}

func HashToStakeID(hash *BlockHash) []byte {
	stakeID := make([]byte, btcec.PubKeyBytesLenCompressed)
	// We need to make the post hash into a uniform 33-byte thing so that
	// it doesn't conflict with public key stake ids.
	copy(stakeID, hash[:])
	stakeID[btcec.PubKeyBytesLenCompressed-1] = 0x00

	return stakeID
}

func StakeIDToHash(stakeID []byte) *BlockHash {
	hash := &BlockHash{}
	copy(hash[:], stakeID[:HashSizeBytes])
	return hash
}

func DBDeletePostEntryMappingsWithTxn(
	txn *badger.Txn, postHash *BlockHash, params *BitCloutParams) error {

	// First pull up the mapping that texists for the post hash passed in.
	// If one doesn't exist then there's nothing to do.
	postEntry := DBGetPostEntryByPostHashWithTxn(txn, postHash)
	if postEntry == nil {
		return nil
	}

	// When a post exists, delete the mapping for the post.
	if err := txn.Delete(_dbKeyForPostEntryHash(postHash)); err != nil {
		return errors.Wrapf(err, "DbDeletePostEntryMappingsWithTxn: Deleting "+
			"post mapping for post hash %v", postHash)
	}

	// If the post is a comment we store it in a separate index. Comments are
	// technically posts but they really should be treated as their own entity.
	// The only reason they're not actually implemented that way is so that we
	// get code re-use.
	isComment := len(postEntry.ParentStakeID) == HashSizeBytes
	if isComment {
		// Extend the parent stake ID, which is a block hash, to 33 bytes, which
		// is the length of a public key and the standard length we use for this
		// key.
		extendedStakeID := append([]byte{}, postEntry.ParentStakeID...)
		extendedStakeID = append(extendedStakeID, 0x00)
		parentStakeIDKey := _dbKeyForCommentParentStakeIDToPostHash(
			extendedStakeID, postEntry.TimestampNanos, postEntry.PostHash)
		if err := txn.Delete(parentStakeIDKey); err != nil {

			return errors.Wrapf(err, "DbDeletePostEntryMappingsWithTxn: Problem "+
				"deleting mapping for comment: %v: %v", postEntry, err)
		}
	} else {
		if err := txn.Delete(_dbKeyForPosterPublicKeyTimestampPostHash(
			postEntry.PosterPublicKey, postEntry.TimestampNanos, postEntry.PostHash)); err != nil {

			return errors.Wrapf(err, "DbDeletePostEntryMappingsWithTxn: Deleting "+
				"public key mapping for post hash %v: %v", postHash, err)
		}
		if err := txn.Delete(_dbKeyForTstampPostHash(
			postEntry.TimestampNanos, postEntry.PostHash)); err != nil {

			return errors.Wrapf(err, "DbDeletePostEntryMappingsWithTxn: Deleting "+
				"tstamp mapping for post hash %v: %v", postHash, err)
		}
		if err := txn.Delete(_dbKeyForCreatorBpsPostHash(
			postEntry.CreatorBasisPoints, postEntry.PostHash)); err != nil {

			return errors.Wrapf(err, "DbDeletePostEntryMappingsWithTxn: Deleting "+
				"creatorBps mapping for post hash %v: %v", postHash, err)
		}
		if err := txn.Delete(_dbKeyForStakeMultipleBpsPostHash(
			postEntry.StakeMultipleBasisPoints, postEntry.PostHash)); err != nil {

			return errors.Wrapf(err, "DbDeletePostEntryMappingsWithTxn: Deleting "+
				"stakeMultiple mapping for post hash %v: %v", postHash, err)
		}

		// Delete the stats for the post.
		stakeStats := GetStakeEntryStats(postEntry.StakeEntry, params)
		if err := txn.Delete(_dbGetStakeIDPostDBKey(
			postEntry.PostHash, stakeStats.TotalStakeNanos)); err != nil {

			return errors.Wrapf(err, "DbDeletePostEntryMappingsWithTxn: Deleting "+
				"StakeAmountNanos mapping for post hash %v", postHash)
		}

		// Delete the reclout entries for the post.
		if IsVanillaReclout(postEntry) {
			if err := txn.Delete(
				_dbKeyForReclouterPubKeyRecloutedPostHashToRecloutPostHash(postEntry.PosterPublicKey, *postEntry.RecloutedPostHash)); err != nil {
				return errors.Wrapf(err, "DbDeletePostEntryMappingsWithTxn: Error problem deleting mapping for recloutPostHash to ReclouterPubKey: %v", err)
			}
		}
	}

	return nil
}

func DBDeletePostEntryMappings(
	handle *badger.DB, postHash *BlockHash, params *BitCloutParams) error {

	return handle.Update(func(txn *badger.Txn) error {
		return DBDeletePostEntryMappingsWithTxn(txn, postHash, params)
	})
}

func DBPutPostEntryMappingsWithTxn(
	txn *badger.Txn, postEntry *PostEntry, params *BitCloutParams) error {

	postDataBuf := bytes.NewBuffer([]byte{})
	gob.NewEncoder(postDataBuf).Encode(postEntry)

	if err := txn.Set(_dbKeyForPostEntryHash(
		postEntry.PostHash), postDataBuf.Bytes()); err != nil {

		return errors.Wrapf(err, "DbPutPostEntryMappingsWithTxn: Problem "+
			"adding mapping for post: %v", postEntry.PostHash)
	}

	// If the post is a comment we store it in a separate index. Comments are
	// technically posts but they really should be treated as their own entity.
	// The only reason they're not actually implemented that way is so that we
	// get code re-use.
	isComment := len(postEntry.ParentStakeID) != 0
	if isComment {
		// Extend the parent stake ID, which is a block hash, to 33 bytes, which
		// is the length of a public key and the standard length we use for this
		// key.
		extendedStakeID := append([]byte{}, postEntry.ParentStakeID...)
		if len(extendedStakeID) == HashSizeBytes {
			extendedStakeID = append(extendedStakeID, 0x00)
		}
		if len(extendedStakeID) != btcec.PubKeyBytesLenCompressed {
			return fmt.Errorf("DbPutPostEntryMappingsWithTxn: extended "+
				"ParentStakeID %#v must have length %v",
				extendedStakeID, btcec.PubKeyBytesLenCompressed)
		}
		parentStakeIDKey := _dbKeyForCommentParentStakeIDToPostHash(
			extendedStakeID, postEntry.TimestampNanos, postEntry.PostHash)
		if err := txn.Set(parentStakeIDKey, []byte{}); err != nil {

			return errors.Wrapf(err, "DbPutPostEntryMappingsWithTxn: Problem "+
				"adding mapping for comment: %v: %v", postEntry, err)
		}

	} else {
		if err := txn.Set(_dbKeyForPosterPublicKeyTimestampPostHash(
			postEntry.PosterPublicKey, postEntry.TimestampNanos, postEntry.PostHash), []byte{}); err != nil {

			return errors.Wrapf(err, "DbPutPostEntryMappingsWithTxn: Problem "+
				"adding mapping for public key: %v: %v", postEntry, err)
		}
		if err := txn.Set(_dbKeyForTstampPostHash(
			postEntry.TimestampNanos, postEntry.PostHash), []byte{}); err != nil {

			return errors.Wrapf(err, "DbPutPostEntryMappingsWithTxn: Problem "+
				"adding mapping for tstamp: %v", postEntry)
		}
		if err := txn.Set(_dbKeyForCreatorBpsPostHash(
			postEntry.CreatorBasisPoints, postEntry.PostHash), []byte{}); err != nil {

			return errors.Wrapf(err, "DbPutPostEntryMappingsWithTxn: Problem "+
				"adding mapping for creatorBps: %v", postEntry)
		}
		if err := txn.Set(_dbKeyForStakeMultipleBpsPostHash(
			postEntry.StakeMultipleBasisPoints, postEntry.PostHash), []byte{}); err != nil {

			return errors.Wrapf(err, "DbPutPostEntryMappingsWithTxn: Problem "+
				"adding mapping for stakeMultipleBps: %v", postEntry)
		}

		// Get stats for the post.
		// <prefix | PostType | AmountStaked | PostHash> -> <>
		stakeStats := GetStakeEntryStats(postEntry.StakeEntry, params)
		if err := txn.Set(
			_dbGetStakeIDPostDBKey(
				postEntry.PostHash, stakeStats.TotalStakeNanos), []byte{}); err != nil {

			return errors.Wrapf(err, "DbPutPostEntryMappingsWithTxn: Problem "+
				"adding mapping for stakeMultipleBps: %v", postEntry)
		}
	}
	// We treat reclouting the same for both comments and posts.
	// We only store reclout entry mappings for vanilla reclouts
	if IsVanillaReclout(postEntry) {
		recloutEntry := RecloutEntry{
			RecloutPostHash:   postEntry.PostHash,
			RecloutedPostHash: postEntry.RecloutedPostHash,
			ReclouterPubKey:   postEntry.PosterPublicKey,
		}
		recloutDataBuf := bytes.NewBuffer([]byte{})
		gob.NewEncoder(recloutDataBuf).Encode(recloutEntry)
		if err := txn.Set(
			_dbKeyForReclouterPubKeyRecloutedPostHashToRecloutPostHash(postEntry.PosterPublicKey, *postEntry.RecloutedPostHash),
			recloutDataBuf.Bytes()); err != nil {
			return errors.Wrapf(err, "DbPutPostEntryMappingsWithTxn: Error problem adding mapping for recloutPostHash to ReclouterPubKey: %v", err)
		}
	}
	return nil
}

func DBPutPostEntryMappings(handle *badger.DB, postEntry *PostEntry, params *BitCloutParams) error {

	return handle.Update(func(txn *badger.Txn) error {
		return DBPutPostEntryMappingsWithTxn(txn, postEntry, params)
	})
}

// Specifying minTimestampNanos gives you all posts after minTimestampNanos
// Pass minTimestampNanos = 0 && maxTimestampNanos = 0 if you want all posts
// Setting maxTimestampNanos = 0, will default maxTimestampNanos to the current time.
func DBGetAllPostsAndCommentsForPublicKeyOrderedByTimestamp(
	handle *badger.DB, publicKey []byte, fetchEntries bool, minTimestampNanos uint64, maxTimestampNanos uint64) (
	_tstamps []uint64, _postAndCommentHashes []*BlockHash, _postAndCommentEntries []*PostEntry, _err error) {

	tstampsFetched := []uint64{}
	postAndCommentHashesFetched := []*BlockHash{}
	postAndCommentEntriesFetched := []*PostEntry{}
	dbPrefixx := append([]byte{}, _PrefixPosterPublicKeyTimestampPostHash...)
	dbPrefixx = append(dbPrefixx, publicKey...)

	err := handle.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions

		opts.PrefetchValues = false

		// Go in reverse order since a larger count is better.
		opts.Reverse = true

		it := txn.NewIterator(opts)
		defer it.Close()
		// Since we iterate backwards, the prefix must be bigger than all possible
		// timestamps that could actually exist. We use eight bytes since the timestamp is
		// encoded as a 64-bit big-endian byte slice, which will be eight bytes long.
		maxBigEndianUint64Bytes := []byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}
		prefix := append(dbPrefixx, maxBigEndianUint64Bytes...)

		// If we have a maxTimeStamp, we use that instead of the maxBigEndianUint64.
		if maxTimestampNanos != 0 {
			prefix = append(dbPrefixx, EncodeUint64(maxTimestampNanos)...)
		}

		for it.Seek(prefix); it.ValidForPrefix(dbPrefixx); it.Next() {
			rawKey := it.Item().Key()

			// Key should be
			// [prefix][posterPublicKey][Timestamp][PostHash]

			// Pull out the relevant fields
			timestampSizeBytes := 8
			keyWithoutPrefix := rawKey[1:]
			//posterPublicKey := keyWithoutPrefix[:HashSizeBytes]
			publicKeySizeBytes := HashSizeBytes + 1
			tstampNanos := DecodeUint64(keyWithoutPrefix[publicKeySizeBytes:(publicKeySizeBytes + timestampSizeBytes)])

			postHash := &BlockHash{}
			copy(postHash[:], keyWithoutPrefix[(publicKeySizeBytes+timestampSizeBytes):])

			if tstampNanos < minTimestampNanos {
				break
			}

			tstampsFetched = append(tstampsFetched, tstampNanos)
			postAndCommentHashesFetched = append(postAndCommentHashesFetched, postHash)
		}
		return nil
	})
	if err != nil {
		return nil, nil, nil, err
	}

	if !fetchEntries {
		return tstampsFetched, postAndCommentHashesFetched, nil, nil
	}

	for _, postHash := range postAndCommentHashesFetched {
		postEntry := DBGetPostEntryByPostHash(handle, postHash)
		if postEntry == nil {
			return nil, nil, nil, fmt.Errorf("DBGetPostEntryByPostHash: "+
				"PostHash %v does not have corresponding entry", postHash)
		}
		postAndCommentEntriesFetched = append(postAndCommentEntriesFetched, postEntry)
	}

	return tstampsFetched, postAndCommentHashesFetched, postAndCommentEntriesFetched, nil
}

// DBGetAllPostsByTstamp returns all the posts in the db with the newest
// posts first.
//
// TODO(performance): This currently fetches all posts. We should implement
// some kind of pagination instead though.
func DBGetAllPostsByTstamp(handle *badger.DB, fetchEntries bool) (
	_tstamps []uint64, _postHashes []*BlockHash, _postEntries []*PostEntry, _err error) {

	tstampsFetched := []uint64{}
	postHashesFetched := []*BlockHash{}
	postEntriesFetched := []*PostEntry{}
	dbPrefixx := append([]byte{}, _PrefixTstampNanosPostHash...)

	err := handle.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions

		opts.PrefetchValues = false

		// Go in reverse order since a larger count is better.
		opts.Reverse = true

		it := txn.NewIterator(opts)
		defer it.Close()
		// Since we iterate backwards, the prefix must be bigger than all possible
		// timestamps that could actually exist. We use eight bytes since the timestamp is
		// encoded as a 64-bit big-endian byte slice, which will be eight bytes long.
		maxBigEndianUint64Bytes := []byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}
		prefix := append(dbPrefixx, maxBigEndianUint64Bytes...)
		for it.Seek(prefix); it.ValidForPrefix(dbPrefixx); it.Next() {
			rawKey := it.Item().Key()

			// Strip the prefix off the key and check its length. If it contains
			// a big-endian uint64 then it should be at least eight bytes.
			tstampPostHashKey := rawKey[1:]
			uint64BytesLen := len(maxBigEndianUint64Bytes)
			if len(tstampPostHashKey) != uint64BytesLen+HashSizeBytes {
				return fmt.Errorf("DBGetAllPostsByTstamp: Invalid key "+
					"length %d should be at least %d", len(tstampPostHashKey),
					uint64BytesLen+HashSizeBytes)
			}

			tstampNanos := DecodeUint64(tstampPostHashKey[:uint64BytesLen])

			// Appended to the tstamp should be the post hash so extract it here.
			postHash := &BlockHash{}
			copy(postHash[:], tstampPostHashKey[uint64BytesLen:])

			tstampsFetched = append(tstampsFetched, tstampNanos)
			postHashesFetched = append(postHashesFetched, postHash)
		}
		return nil
	})
	if err != nil {
		return nil, nil, nil, err
	}

	if !fetchEntries {
		return tstampsFetched, postHashesFetched, nil, nil
	}

	for _, postHash := range postHashesFetched {
		postEntry := DBGetPostEntryByPostHash(handle, postHash)
		if postEntry == nil {
			return nil, nil, nil, fmt.Errorf("DBGetPostEntryByPostHash: "+
				"PostHash %v does not have corresponding entry", postHash)
		}
		postEntriesFetched = append(postEntriesFetched, postEntry)
	}

	return tstampsFetched, postHashesFetched, postEntriesFetched, nil
}

// DBGetCommentPostHashesForParentStakeID returns all the comments, which are indexed by their
// stake ID rather than by their timestamp.
//
// TODO(performance): This currently fetches all comments. We should implement
// something where we only get the comments for particular posts instead.
func DBGetCommentPostHashesForParentStakeID(
	handle *badger.DB, stakeIDXXX []byte, fetchEntries bool) (
	_tstamps []uint64, _commentPostHashes []*BlockHash, _commentPostEntryes []*PostEntry, _err error) {

	tstampsFetched := []uint64{}
	commentPostHashes := []*BlockHash{}
	commentEntriesFetched := []*PostEntry{}
	dbPrefixx := append([]byte{}, _PrefixCommentParentStakeIDToPostHash...)
	dbPrefixx = append(dbPrefixx, stakeIDXXX...)

	err := handle.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions

		opts.PrefetchValues = false

		it := txn.NewIterator(opts)
		defer it.Close()
		// Since we iterate backwards, the prefix must be bigger than all possible
		// counts that could actually exist. We use eight bytes since the count is
		// encoded as a 64-bit big-endian byte slice, which will be eight bytes long.
		maxBigEndianUint64Bytes := []byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}
		//prefix := append(dbPrefixx, maxBigEndianUint64Bytes...)
		prefix := dbPrefixx
		for it.Seek(prefix); it.ValidForPrefix(dbPrefixx); it.Next() {
			rawKey := it.Item().Key()

			// Strip the prefix off the key and check its length. It should contain
			// a 33-byte stake id, an 8 byte tstamp, and a 32 byte comment hash.
			stakeIDTstampPostHashKey := rawKey[1:]
			uint64BytesLen := len(maxBigEndianUint64Bytes)
			if len(stakeIDTstampPostHashKey) != btcec.PubKeyBytesLenCompressed+uint64BytesLen+HashSizeBytes {
				return fmt.Errorf("DBGetCommentPostHashesForParentStakeID: Invalid key "+
					"length %d should be at least %d", len(stakeIDTstampPostHashKey),
					btcec.PubKeyBytesLenCompressed+uint64BytesLen+HashSizeBytes)
			}

			//stakeID := stakeIDTstampPostHashKey[:btcec.PubKeyBytesLenCompressed]
			tstampNanos := DecodeUint64(stakeIDTstampPostHashKey[btcec.PubKeyBytesLenCompressed : btcec.PubKeyBytesLenCompressed+uint64BytesLen])

			commentPostHashBytes := stakeIDTstampPostHashKey[btcec.PubKeyBytesLenCompressed+uint64BytesLen:]
			commentPostHash := &BlockHash{}
			copy(commentPostHash[:], commentPostHashBytes)

			//stakeIDsFetched = append(stakeIDsFetched, stakeID)
			tstampsFetched = append(tstampsFetched, tstampNanos)
			commentPostHashes = append(commentPostHashes, commentPostHash)
		}
		return nil
	})
	if err != nil {
		return nil, nil, nil, err
	}

	if !fetchEntries {
		return tstampsFetched, commentPostHashes, nil, nil
	}

	for _, postHash := range commentPostHashes {
		postEntry := DBGetPostEntryByPostHash(handle, postHash)
		if postEntry == nil {
			return nil, nil, nil, fmt.Errorf("DBGetCommentPostHashesForParentStakeID: "+
				"PostHash %v does not have corresponding entry", postHash)
		}
		commentEntriesFetched = append(commentEntriesFetched, postEntry)
	}

	return tstampsFetched, commentPostHashes, commentEntriesFetched, nil
}

// ======================================================================================
// Profile code
// ======================================================================================
func _dbKeyForPKIDToProfileEntry(pkid *PKID) []byte {
	prefixCopy := append([]byte{}, _PrefixPKIDToProfileEntry...)
	key := append(prefixCopy, pkid[:]...)
	return key
}
func _dbKeyForProfileUsernameToPKID(nonLowercaseUsername []byte) []byte {
	// Make a copy to avoid multiple calls to this function re-using the same slice.
	key := append([]byte{}, _PrefixProfileUsernameToPKID...)
	// Always lowercase the username when we use it as a key in our db. This allows
	// us to check uniqueness in a case-insensitive way.
	lowercaseUsername := []byte(strings.ToLower(string(nonLowercaseUsername)))
	key = append(key, lowercaseUsername...)
	return key
}

// This is the key we use to sort profiles by their amount of BitClout locked
func _dbKeyForCreatorBitCloutLockedNanosCreatorPKID(bitCloutLockedNanos uint64, pkid *PKID) []byte {
	key := append([]byte{}, _PrefixCreatorBitCloutLockedNanosCreatorPKID...)
	key = append(key, EncodeUint64(bitCloutLockedNanos)...)
	key = append(key, pkid[:]...)
	return key
}

func DBGetPKIDForUsernameWithTxn(
	txn *badger.Txn, username []byte) *PKID {

	key := _dbKeyForProfileUsernameToPKID(username)
	profileEntryItem, err := txn.Get(key)
	if err != nil {
		return nil
	}
	pkidBytes, err := profileEntryItem.ValueCopy(nil)
	if err != nil {
		glog.Errorf("DBGetProfileEntryForUsernameWithTxn: Problem reading "+
			"public key for username %v: %v", string(username), err)
		return nil
	}

	return PublicKeyToPKID(pkidBytes)
}

func DBGetPKIDForUsername(db *badger.DB, username []byte) *PKID {
	var ret *PKID
	db.View(func(txn *badger.Txn) error {
		ret = DBGetPKIDForUsernameWithTxn(txn, username)
		return nil
	})
	return ret
}

func DBGetProfileEntryForUsernameWithTxn(
	txn *badger.Txn, username []byte) *ProfileEntry {

	pkid := DBGetPKIDForUsernameWithTxn(txn, username)
	if pkid == nil {
		return nil
	}

	return DBGetProfileEntryForPKIDWithTxn(txn, pkid)
}

func DBGetProfileEntryForUsername(db *badger.DB, username []byte) *ProfileEntry {
	var ret *ProfileEntry
	db.View(func(txn *badger.Txn) error {
		ret = DBGetProfileEntryForUsernameWithTxn(txn, username)
		return nil
	})
	return ret
}

func DBGetProfileEntryForPKIDWithTxn(
	txn *badger.Txn, pkid *PKID) *ProfileEntry {

	key := _dbKeyForPKIDToProfileEntry(pkid)
	profileEntryObj := &ProfileEntry{}
	profileEntryItem, err := txn.Get(key)
	if err != nil {
		return nil
	}
	err = profileEntryItem.Value(func(valBytes []byte) error {
		return gob.NewDecoder(bytes.NewReader(valBytes)).Decode(profileEntryObj)
	})
	if err != nil {
		glog.Errorf("DBGetProfileEntryForPubKeyWithTxnhWithTxn: Problem reading "+
			"ProfileEntry for PKID %v", pkid)
		return nil
	}
	return profileEntryObj
}

func DBGetProfileEntryForPKID(db *badger.DB, pkid *PKID) *ProfileEntry {
	var ret *ProfileEntry
	db.View(func(txn *badger.Txn) error {
		ret = DBGetProfileEntryForPKIDWithTxn(txn, pkid)
		return nil
	})
	return ret
}

func DBDeleteProfileEntryMappingsWithTxn(
	txn *badger.Txn, pkid *PKID, params *BitCloutParams) error {

	// First pull up the mapping that exists for the profile pub key passed in.
	// If one doesn't exist then there's nothing to do.
	profileEntry := DBGetProfileEntryForPKIDWithTxn(txn, pkid)
	if profileEntry == nil {
		return nil
	}

	// When a profile exists, delete the pkid mapping for the profile.
	if err := txn.Delete(_dbKeyForPKIDToProfileEntry(pkid)); err != nil {
		return errors.Wrapf(err, "DbDeleteProfileEntryMappingsWithTxn: Deleting "+
			"profile mapping for profile PKID: %v",
			PkToString(pkid[:], params))
	}

	if err := txn.Delete(
		_dbKeyForProfileUsernameToPKID(profileEntry.Username)); err != nil {

		return errors.Wrapf(err, "DbDeleteProfileEntryMappingsWithTxn: Deleting "+
			"username mapping for profile username %v", string(profileEntry.Username))
	}

	// The coin clout mapping
	if err := txn.Delete(
		_dbKeyForCreatorBitCloutLockedNanosCreatorPKID(
			profileEntry.BitCloutLockedNanos, pkid)); err != nil {

		return errors.Wrapf(err, "DbDeleteProfileEntryMappingsWithTxn: Deleting "+
			"coin mapping for profile username %v", string(profileEntry.Username))
	}

	return nil
}

func DBDeleteProfileEntryMappings(
	handle *badger.DB, pkid *PKID, params *BitCloutParams) error {

	return handle.Update(func(txn *badger.Txn) error {
		return DBDeleteProfileEntryMappingsWithTxn(txn, pkid, params)
	})
}

func DBPutProfileEntryMappingsWithTxn(
	txn *badger.Txn, profileEntry *ProfileEntry, pkid *PKID, params *BitCloutParams) error {

	profileDataBuf := bytes.NewBuffer([]byte{})
	gob.NewEncoder(profileDataBuf).Encode(profileEntry)

	// Set the main PKID -> profile entry mapping.
	if err := txn.Set(_dbKeyForPKIDToProfileEntry(pkid), profileDataBuf.Bytes()); err != nil {

		return errors.Wrapf(err, "DbPutProfileEntryMappingsWithTxn: Problem "+
			"adding mapping for profile: %v", PkToString(pkid[:], params))
	}

	// Username
	if err := txn.Set(
		_dbKeyForProfileUsernameToPKID(profileEntry.Username),
		pkid[:]); err != nil {

		return errors.Wrapf(err, "DbPutProfileEntryMappingsWithTxn: Problem "+
			"adding mapping for profile with username: %v", string(profileEntry.Username))
	}

	// The coin clout mapping
	if err := txn.Set(
		_dbKeyForCreatorBitCloutLockedNanosCreatorPKID(
			profileEntry.BitCloutLockedNanos, pkid), []byte{}); err != nil {

		return errors.Wrapf(err, "DbPutProfileEntryMappingsWithTxn: Problem "+
			"adding mapping for profile coin: ")
	}

	return nil
}

func DBPutProfileEntryMappings(
	handle *badger.DB, profileEntry *ProfileEntry, pkid *PKID, params *BitCloutParams) error {

	return handle.Update(func(txn *badger.Txn) error {
		return DBPutProfileEntryMappingsWithTxn(txn, profileEntry, pkid, params)
	})
}

// DBGetAllProfilesByCoinValue returns all the profiles in the db with the
// highest coin values first.
//
// TODO(performance): This currently fetches all profiles. We should implement
// some kind of pagination instead though.
func DBGetAllProfilesByCoinValue(handle *badger.DB, fetchEntries bool) (
	_lockedBitCloutNanos []uint64, _profilePublicKeys []*PKID,
	_profileEntries []*ProfileEntry, _err error) {

	lockedBitCloutNanosFetched := []uint64{}
	profilePublicKeysFetched := []*PKID{}
	profileEntriesFetched := []*ProfileEntry{}
	dbPrefixx := append([]byte{}, _PrefixCreatorBitCloutLockedNanosCreatorPKID...)

	err := handle.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions

		opts.PrefetchValues = false

		// Go in reverse order since a larger count is better.
		opts.Reverse = true

		it := txn.NewIterator(opts)
		defer it.Close()
		// Since we iterate backwards, the prefix must be bigger than all possible
		// counts that could actually exist. We use eight bytes since the count is
		// encoded as a 64-bit big-endian byte slice, which will be eight bytes long.
		maxBigEndianUint64Bytes := []byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}
		prefix := append(dbPrefixx, maxBigEndianUint64Bytes...)
		for it.Seek(prefix); it.ValidForPrefix(dbPrefixx); it.Next() {
			rawKey := it.Item().Key()

			// Strip the prefix off the key and check its length. If it contains
			// a big-endian uint64 then it should be at least eight bytes.
			lockedBitCloutPubKeyConcatKey := rawKey[1:]
			uint64BytesLen := len(maxBigEndianUint64Bytes)
			expectedLength := uint64BytesLen + btcec.PubKeyBytesLenCompressed
			if len(lockedBitCloutPubKeyConcatKey) != expectedLength {
				return fmt.Errorf("DBGetAllProfilesByLockedBitClout: Invalid key "+
					"length %d should be at least %d", len(lockedBitCloutPubKeyConcatKey),
					expectedLength)
			}

			lockedBitCloutNanos := DecodeUint64(lockedBitCloutPubKeyConcatKey[:uint64BytesLen])

			// Appended to the stake should be the profile pub key so extract it here.
			profilePKID := make([]byte, btcec.PubKeyBytesLenCompressed)
			copy(profilePKID[:], lockedBitCloutPubKeyConcatKey[uint64BytesLen:])

			lockedBitCloutNanosFetched = append(lockedBitCloutNanosFetched, lockedBitCloutNanos)
			profilePublicKeysFetched = append(profilePublicKeysFetched, PublicKeyToPKID(profilePKID))
		}
		return nil
	})
	if err != nil {
		return nil, nil, nil, err
	}

	if !fetchEntries {
		return lockedBitCloutNanosFetched, profilePublicKeysFetched, nil, nil
	}

	for _, profilePKID := range profilePublicKeysFetched {
		profileEntry := DBGetProfileEntryForPKID(handle, profilePKID)
		if profileEntry == nil {
			return nil, nil, nil, fmt.Errorf("DBGetAllProfilesByLockedBitClout: "+
				"ProfilePubKey %v does not have corresponding entry",
				PkToStringBoth(profilePKID[:]))
		}
		profileEntriesFetched = append(profileEntriesFetched, profileEntry)
	}

	return lockedBitCloutNanosFetched, profilePublicKeysFetched, profileEntriesFetched, nil
}

// =====================================================================================
// Creator coin balance entry code
// =====================================================================================
func _dbKeyForHODLerPKIDCreatorPKIDToBalanceEntry(hodlerPKID *PKID, creatorPKID *PKID) []byte {
	key := append([]byte{}, _PrefixHODLerPKIDCreatorPKIDToBalanceEntry...)
	key = append(key, hodlerPKID[:]...)
	key = append(key, creatorPKID[:]...)
	return key
}
func _dbKeyForCreatorPKIDHODLerPKIDToBalanceEntry(creatorPKID *PKID, hodlerPKID *PKID) []byte {
	key := append([]byte{}, _PrefixCreatorPKIDHODLerPKIDToBalanceEntry...)
	key = append(key, creatorPKID[:]...)
	key = append(key, hodlerPKID[:]...)
	return key
}

func DBGetCreatorCoinBalanceEntryForHODLerAndCreatorPKIDsWithTxn(
	txn *badger.Txn, hodlerPKID *PKID, creatorPKID *PKID) *BalanceEntry {

	key := _dbKeyForHODLerPKIDCreatorPKIDToBalanceEntry(hodlerPKID, creatorPKID)
	balanceEntryObj := &BalanceEntry{}
	balanceEntryItem, err := txn.Get(key)
	if err != nil {
		return nil
	}
	err = balanceEntryItem.Value(func(valBytes []byte) error {
		return gob.NewDecoder(bytes.NewReader(valBytes)).Decode(balanceEntryObj)
	})
	if err != nil {
		glog.Errorf("DBGetCreatorCoinBalanceEntryForHODLerAndCreatorPubKeysWithTxn: Problem reading "+
			"BalanceEntry for PKIDs %v %v",
			PkToStringBoth(hodlerPKID[:]), PkToStringBoth(creatorPKID[:]))
		return nil
	}
	return balanceEntryObj
}

func DBGetCreatorCoinBalanceEntryForHODLerAndCreatorPKIDs(
	handle *badger.DB, hodlerPKID *PKID, creatorPKID *PKID) *BalanceEntry {

	var ret *BalanceEntry
	handle.View(func(txn *badger.Txn) error {
		ret = DBGetCreatorCoinBalanceEntryForHODLerAndCreatorPKIDsWithTxn(
			txn, hodlerPKID, creatorPKID)
		return nil
	})
	return ret
}

func DBGetCreatorCoinBalanceEntryForCreatorPKIDAndHODLerPubKeyWithTxn(
	txn *badger.Txn, creatorPKID *PKID, hodlerPKID *PKID) *BalanceEntry {

	key := _dbKeyForCreatorPKIDHODLerPKIDToBalanceEntry(creatorPKID, hodlerPKID)
	balanceEntryObj := &BalanceEntry{}
	balanceEntryItem, err := txn.Get(key)
	if err != nil {
		return nil
	}
	err = balanceEntryItem.Value(func(valBytes []byte) error {
		return gob.NewDecoder(bytes.NewReader(valBytes)).Decode(balanceEntryObj)
	})
	if err != nil {
		glog.Errorf("DBGetCreatorCoinBalanceEntryForCreatorPubKeyAndHODLerPubKeyWithTxn: Problem reading "+
			"BalanceEntry for PKIDs %v %v",
			PkToStringBoth(hodlerPKID[:]), PkToStringBoth(creatorPKID[:]))
		return nil
	}
	return balanceEntryObj
}

func DBDeleteCreatorCoinBalanceEntryMappingsWithTxn(
	txn *badger.Txn, hodlerPKID *PKID, creatorPKID *PKID,
	params *BitCloutParams) error {

	// First pull up the mappings that exists for the keys passed in.
	// If one doesn't exist then there's nothing to do.
	balanceEntry := DBGetCreatorCoinBalanceEntryForHODLerAndCreatorPKIDsWithTxn(
		txn, hodlerPKID, creatorPKID)
	if balanceEntry == nil {
		return nil
	}

	// When an entry exists, delete the mappings for it.
	if err := txn.Delete(_dbKeyForHODLerPKIDCreatorPKIDToBalanceEntry(hodlerPKID, creatorPKID)); err != nil {
		return errors.Wrapf(err, "DbDeleteCreatorCoinBalanceEntryMappingsWithTxn: Deleting "+
			"mappings with keys: %v %v",
			PkToStringBoth(hodlerPKID[:]), PkToStringBoth(creatorPKID[:]))
	}
	if err := txn.Delete(_dbKeyForCreatorPKIDHODLerPKIDToBalanceEntry(creatorPKID, hodlerPKID)); err != nil {
		return errors.Wrapf(err, "DbDeleteCreatorCoinBalanceEntryMappingsWithTxn: Deleting "+
			"mappings with keys: %v %v",
			PkToStringBoth(hodlerPKID[:]), PkToStringBoth(creatorPKID[:]))
	}

	// Note: We don't update the CreatorBitCloutLockedNanosCreatorPubKeyIIndex
	// because we expect that the caller is keeping the individual holdings in
	// sync with the "total" coins stored in the profile.

	return nil
}

func DBDeleteCreatorCoinBalanceEntryMappings(
	handle *badger.DB, hodlerPKID *PKID, creatorPKID *PKID,
	params *BitCloutParams) error {

	return handle.Update(func(txn *badger.Txn) error {
		return DBDeleteCreatorCoinBalanceEntryMappingsWithTxn(
			txn, hodlerPKID, creatorPKID, params)
	})
}

func DBPutCreatorCoinBalanceEntryMappingsWithTxn(
	txn *badger.Txn, balanceEntry *BalanceEntry,
	params *BitCloutParams) error {

	balanceEntryDataBuf := bytes.NewBuffer([]byte{})
	gob.NewEncoder(balanceEntryDataBuf).Encode(balanceEntry)

	// Set the forward direction for the HODLer
	if err := txn.Set(_dbKeyForHODLerPKIDCreatorPKIDToBalanceEntry(
		balanceEntry.HODLerPKID, balanceEntry.CreatorPKID),
		balanceEntryDataBuf.Bytes()); err != nil {

		return errors.Wrapf(err, "DbPutCreatorCoinBalanceEntryMappingsWithTxn: Problem "+
			"adding forward mappings for pub keys: %v %v",
			PkToStringBoth(balanceEntry.HODLerPKID[:]),
			PkToStringBoth(balanceEntry.CreatorPKID[:]))
	}

	// Set the reverse direction for the creator
	if err := txn.Set(_dbKeyForCreatorPKIDHODLerPKIDToBalanceEntry(
		balanceEntry.CreatorPKID, balanceEntry.HODLerPKID),
		balanceEntryDataBuf.Bytes()); err != nil {

		return errors.Wrapf(err, "DbPutCreatorCoinBalanceEntryMappingsWithTxn: Problem "+
			"adding reverse mappings for pub keys: %v %v",
			PkToStringBoth(balanceEntry.HODLerPKID[:]),
			PkToStringBoth(balanceEntry.CreatorPKID[:]))
	}

	return nil
}

func DBPutCreatorCoinBalanceEntryMappings(
	handle *badger.DB, balanceEntry *BalanceEntry, params *BitCloutParams) error {

	return handle.Update(func(txn *badger.Txn) error {
		return DBPutCreatorCoinBalanceEntryMappingsWithTxn(
			txn, balanceEntry, params)
	})
}

// GetSingleBalanceEntryFromPublicKeys fetchs a single balance entry of a holder's creator coin.
// Returns nil if the balance entry never existed.
func GetSingleBalanceEntryFromPublicKeys(holder []byte, creator []byte, utxoView *UtxoView) (*BalanceEntry, error){
	holderPKIDEntry := utxoView.GetPKIDForPublicKey(holder)
	if holderPKIDEntry == nil || holderPKIDEntry.isDeleted {
		return nil, fmt.Errorf("DbGetSingleBalanceEntryFromPublicKeys: holderPKID was nil or deleted; this should never happen")
	}
	holderPKID := holderPKIDEntry.PKID
	creatorPKIDEntry := utxoView.GetPKIDForPublicKey(creator)
	if creatorPKIDEntry == nil || creatorPKIDEntry.isDeleted {
		return nil, fmt.Errorf("DbGetSingleBalanceEntryFromPublicKeys: creatorPKID was nil or deleted; this should never happen")
	}
	creatorPKID := creatorPKIDEntry.PKID

	// Check if there's a balance entry in the view
	balanceEntryMapKey := BalanceEntryMapKey{HODLerPKID: *holderPKID, CreatorPKID: *creatorPKID}
	balanceEntryFromView, _ := utxoView.HODLerPKIDCreatorPKIDToBalanceEntry[balanceEntryMapKey]
	if balanceEntryFromView != nil {
		return balanceEntryFromView, nil
	}

	// Check if there's a balance entry in the database
	balanceEntryFromDb := DbGetBalanceEntry(utxoView.Handle, holderPKID, creatorPKID)
	return balanceEntryFromDb, nil
}

// DbGetBalanceEntry returns a balance entry from the database
func DbGetBalanceEntry(db *badger.DB, holder *PKID, creator *PKID) *BalanceEntry {
	var ret *BalanceEntry
	db.View(func(txn *badger.Txn) error {
		ret = DbGetHolderPKIDCreatorPKIDToBalanceEntryWithTxn(txn, holder, creator)
		return nil
	})
	return ret
}

func DbGetHolderPKIDCreatorPKIDToBalanceEntryWithTxn(txn *badger.Txn, holder *PKID, creator *PKID) *BalanceEntry {
	key := _dbKeyForCreatorPKIDHODLerPKIDToBalanceEntry(creator, holder)
	balanceEntryObj := &BalanceEntry{}
	balanceEntryItem, err := txn.Get(key)
	if err != nil {
		return nil
	}
	err = balanceEntryItem.Value(func(valBytes []byte) error {
		return gob.NewDecoder(bytes.NewReader(valBytes)).Decode(balanceEntryObj)
	})
	if err != nil {
		glog.Errorf("DbGetReclouterPubKeyRecloutedPostHashToRecloutedPostMappingWithTxn: Problem decoding "+
			"balance entry for holder %v and creator %v", PkToStringMainnet(PKIDToPublicKey(holder)), PkToStringMainnet(PKIDToPublicKey(creator)))
		return nil
	}
	return balanceEntryObj
}

// DbGetBalanceEntriesHodlingYou fetchs the BalanceEntries that the passed in pkid hodls.
func DbGetBalanceEntriesYouHodl(pkid *PKIDEntry, fetchProfiles bool, filterOutZeroBalances bool, utxoView *UtxoView) (
	_entriesYouHodl []*BalanceEntry,
	_profilesYouHodl []*ProfileEntry,
	_err error) {
	handle := utxoView.Handle
	// Get the balance entries for the coins that *you hodl*
	balanceEntriesYouHodl := []*BalanceEntry{}
	{
		prefix := append([]byte{}, _PrefixHODLerPKIDCreatorPKIDToBalanceEntry...)
		keyPrefix := append(prefix, pkid.PKID[:]...)
		_, entryByteStringsFound := _enumerateKeysForPrefix(
			handle, keyPrefix)
		for _, byteString := range entryByteStringsFound {
			currentEntry := &BalanceEntry{}
			gob.NewDecoder(bytes.NewReader(byteString)).Decode(currentEntry)
			if filterOutZeroBalances && currentEntry.BalanceNanos == 0 {
				continue
			}
			balanceEntriesYouHodl = append(balanceEntriesYouHodl, currentEntry)
		}
	}
	// Optionally fetch all the profile entries as well.
	profilesYouHodl := []*ProfileEntry{}
	if fetchProfiles {
		for _, balanceEntry := range balanceEntriesYouHodl {
			// In this case you're the hodler so the creator is the one whose
			// profile we need to fetch.
			currentProfileEntry := utxoView.GetProfileEntryForPKID(balanceEntry.CreatorPKID)
			profilesYouHodl = append(profilesYouHodl, currentProfileEntry)
		}
	}
	return balanceEntriesYouHodl, profilesYouHodl, nil
}

// DbGetBalanceEntriesHodlingYou fetchs the BalanceEntries that hodl the pkid passed in.
func DbGetBalanceEntriesHodlingYou(pkid *PKIDEntry, fetchProfiles bool, filterOutZeroBalances bool, utxoView *UtxoView) (
	_entriesHodlingYou []*BalanceEntry,
	_profilesHodlingYou []*ProfileEntry,
	_err error) {
	handle := utxoView.Handle
	// Get the balance entries for the coins that *hodl you*
	balanceEntriesThatHodlYou := []*BalanceEntry{}
	{
		prefix := append([]byte{}, _PrefixCreatorPKIDHODLerPKIDToBalanceEntry...)
		keyPrefix := append(prefix, pkid.PKID[:]...)
		_, entryByteStringsFound := _enumerateKeysForPrefix(
			handle, keyPrefix)
		for _, byteString := range entryByteStringsFound {
			currentEntry := &BalanceEntry{}
			gob.NewDecoder(bytes.NewReader(byteString)).Decode(currentEntry)
			if filterOutZeroBalances && currentEntry.BalanceNanos == 0 {
				continue
			}
			balanceEntriesThatHodlYou = append(balanceEntriesThatHodlYou, currentEntry)
		}
	}
	// Optionally fetch all the profile entries as well.
	profilesThatHodlYou := []*ProfileEntry{}
	if fetchProfiles {
		for _, balanceEntry := range balanceEntriesThatHodlYou {
			// In this case you're the creator and the hodler is the one whose
			// profile we need to fetch.
			currentProfileEntry := utxoView.GetProfileEntryForPKID(balanceEntry.HODLerPKID)
			profilesThatHodlYou = append(profilesThatHodlYou, currentProfileEntry)
		}
	}
	return balanceEntriesThatHodlYou, profilesThatHodlYou, nil
}

// DbGetCreatorCoinBalanceEntriesForPubKeyWithTxn finds the BalanceEntries corresponding
// to the public key passed in. It fetched both the entries corresponding to the
// profiles that *you HODL* and the entries corresponding to the profiles that
// *HODL you*.
func DbGetCreatorCoinBalanceEntriesForPubKey(
	pkid *PKIDEntry, fetchProfiles bool, filterOutZeroBalances bool, utxoView *UtxoView) (
	_entriesYouHodl []*BalanceEntry, _entriesThatHodlYou []*BalanceEntry,
	_profilesYouHodl []*ProfileEntry, _profilesThatHodlYou []*ProfileEntry,
	_err error) {

	balanceEntriesYouHodl, profilesYouHodl, err := DbGetBalanceEntriesYouHodl(pkid, fetchProfiles, filterOutZeroBalances, utxoView)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	balanceEntriesThatHodlYou, profilesThatHodlYou, err := DbGetBalanceEntriesHodlingYou(pkid, fetchProfiles, filterOutZeroBalances, utxoView)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	// If there are no errors, return everything we found.
	return balanceEntriesYouHodl, balanceEntriesThatHodlYou, profilesYouHodl, profilesThatHodlYou, nil
}

// =====================================================================================
// End coin balance entry code
// =====================================================================================

// startPrefix specifies a point in the DB at which the iteration should start.
// It doesn't have to map to an exact key because badger will just binary search
// and start right before/after that location.
//
// validForPrefix helps determine when the iteration should stop. The iteration
// stops at the last entry that has this prefix. Setting it to
// an empty byte string would cause the iteration to seek to the beginning of the db,
// whereas setting it to one of the _Prefix bytes would cause the iteration to stop
// at the last entry with that prefix.
//
// maxKeyLen is required so we can pad the key with FF in the case the user wants
// to seek backwards. This is required due to a quirk of badgerdb. It is ignored
// if reverse == false.
//
// numToFetch specifies the number of entries to fetch. If set to zero then it
// fetches all entries that match the validForPrefix passed in.
func DBGetPaginatedKeysAndValuesForPrefixWithTxn(
	dbTxn *badger.Txn, startPrefix []byte, validForPrefix []byte,
	maxKeyLen int, numToFetch int, reverse bool, fetchValues bool) (

	_keysFound [][]byte, _valsFound [][]byte, _err error) {

	keysFound := [][]byte{}
	valsFound := [][]byte{}

	opts := badger.DefaultIteratorOptions

	opts.PrefetchValues = fetchValues

	// Optionally go in reverse order.
	opts.Reverse = reverse

	it := dbTxn.NewIterator(opts)
	defer it.Close()
	prefix := startPrefix
	if reverse {
		// When we iterate backwards, the prefix must be bigger than all possible
		// keys that could actually exist with this prefix. We achieve this by
		// padding the end of the dbPrefixx passed in up to the key length.
		prefix = make([]byte, maxKeyLen)
		for ii := 0; ii < maxKeyLen; ii++ {
			if ii < len(startPrefix) {
				prefix[ii] = startPrefix[ii]
			} else {
				prefix[ii] = 0xFF
			}
		}
	}
	for it.Seek(prefix); it.ValidForPrefix(validForPrefix); it.Next() {
		keyCopy := it.Item().KeyCopy(nil)
		if maxKeyLen != 0 && len(keyCopy) != maxKeyLen {
			return nil, nil, fmt.Errorf(
				"DBGetPaginatedKeysAndValuesForPrefixWithTxn: Invalid key length %v != %v",
				len(keyCopy), maxKeyLen)
		}

		var valCopy []byte
		if fetchValues {
			var err error
			valCopy, err = it.Item().ValueCopy(nil)
			if err != nil {
				return nil, nil, fmt.Errorf("DBGetPaginatedKeysAndValuesForPrefixWithTxn: "+
					"Error fetching value: %v", err)
			}
		}

		keysFound = append(keysFound, keyCopy)
		valsFound = append(valsFound, valCopy)

		if numToFetch != 0 && len(keysFound) == numToFetch {
			break
		}
	}

	// Return whatever we found.
	return keysFound, valsFound, nil
}

func DBGetPaginatedKeysAndValuesForPrefix(
	db *badger.DB, startPrefix []byte, validForPrefix []byte,
	keyLen int, numToFetch int, reverse bool, fetchValues bool) (
	_keysFound [][]byte, _valsFound [][]byte, _err error) {

	keysFound := [][]byte{}
	valsFound := [][]byte{}

	dbErr := db.View(func(txn *badger.Txn) error {
		var err error
		keysFound, valsFound, err = DBGetPaginatedKeysAndValuesForPrefixWithTxn(
			txn, startPrefix, validForPrefix, keyLen,
			numToFetch, reverse, fetchValues)
		if err != nil {
			return fmt.Errorf("DBGetPaginatedKeysAndValuesForPrefix: %v", err)
		}
		return nil
	})
	if dbErr != nil {
		return nil, nil, dbErr
	}

	return keysFound, valsFound, nil
}

func DBGetPaginatedPostsOrderedByTime(
	db *badger.DB, startPostTimestampNanos uint64, startPostHash *BlockHash,
	numToFetch int, fetchPostEntries bool, reverse bool) (
	_postHashes []*BlockHash, _tstampNanos []uint64, _postEntries []*PostEntry,
	_err error) {

	startPostPrefix := append([]byte{}, _PrefixTstampNanosPostHash...)

	if startPostTimestampNanos > 0 {
		startTstampBytes := EncodeUint64(startPostTimestampNanos)
		startPostPrefix = append(startPostPrefix, startTstampBytes...)
	}

	if startPostHash != nil {
		startPostPrefix = append(startPostPrefix, startPostHash[:]...)
	}

	// We fetch in reverse to get the latest posts.
	maxUint64Tstamp := []byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}
	postIndexKeys, _, err := DBGetPaginatedKeysAndValuesForPrefix(
		db, startPostPrefix, _PrefixTstampNanosPostHash, /*validForPrefix*/
		len(_PrefixTstampNanosPostHash)+len(maxUint64Tstamp)+HashSizeBytes, /*keyLen*/
		numToFetch, reverse /*reverse*/, false /*fetchValues*/)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("DBGetPaginatedPostsOrderedByTime: %v", err)
	}

	// Cut the post hashes and timestamps out of the returned keys.
	postHashes := []*BlockHash{}
	tstamps := []uint64{}
	startTstampIndex := len(_PrefixTstampNanosPostHash)
	hashStartIndex := len(_PrefixTstampNanosPostHash) + len(maxUint64Tstamp)
	hashEndIndex := hashStartIndex + HashSizeBytes
	for _, postKeyBytes := range postIndexKeys {
		currentPostHash := &BlockHash{}
		copy(currentPostHash[:], postKeyBytes[hashStartIndex:hashEndIndex])
		postHashes = append(postHashes, currentPostHash)

		tstamps = append(tstamps, DecodeUint64(
			postKeyBytes[startTstampIndex:hashStartIndex]))
	}

	// Fetch the PostEntries if desired.
	var postEntries []*PostEntry
	if fetchPostEntries {
		for _, postHash := range postHashes {
			postEntry := DBGetPostEntryByPostHash(db, postHash)
			if postEntry == nil {
				return nil, nil, nil, fmt.Errorf("DBGetPaginatedPostsOrderedByTime: "+
					"PostHash %v does not have corresponding entry", postHash)
			}
			postEntries = append(postEntries, postEntry)
		}
	}

	return postHashes, tstamps, postEntries, nil
}

func DBGetProfilesByUsernamePrefixAndBitCloutLocked(
	db *badger.DB, usernamePrefix string, utxoView *UtxoView) (
	_profileEntries []*ProfileEntry, _err error) {

	startPrefix := append([]byte{}, _PrefixProfileUsernameToPKID...)
	lowercaseUsernamePrefixString := strings.ToLower(usernamePrefix)
	lowercaseUsernamePrefix := []byte(lowercaseUsernamePrefixString)
	startPrefix = append(startPrefix, lowercaseUsernamePrefix...)

	_, pkidsFound, err := DBGetPaginatedKeysAndValuesForPrefix(
		db /*db*/, startPrefix, /*startPrefix*/
		startPrefix /*validForPrefix*/, 0, /*keyLen (ignored when reverse == false)*/
		0 /*numToFetch (zero fetches all)*/, false, /*reverse*/
		true /*fetchValues*/)
	if err != nil {
		return nil, fmt.Errorf("DBGetProfilesByUsernamePrefixAndBitCloutLocked: %v", err)
	}

	// Have to do this to convert the PKIDs back into public keys
	// TODO: We should clean things up around public keys vs PKIDs
	pubKeysMap := make(map[PkMapKey][]byte)
	for _, pkidBytes := range pkidsFound {
		if len(pkidBytes) != btcec.PubKeyBytesLenCompressed {
			continue
		}
		pkid := &PKID{}
		copy(pkid[:], pkidBytes)
		pubKey := DBGetPublicKeyForPKID(db, pkid)
		if len(pubKey) != 0 {
			pubKeysMap[MakePkMapKey(pubKey)] = pubKey
		}
	}

	for username, profileEntry := range utxoView.ProfileUsernameToProfileEntry {
		if strings.HasPrefix(string(username[:]), lowercaseUsernamePrefixString) {
			pkMapKey := MakePkMapKey(profileEntry.PublicKey)
			pubKeysMap[pkMapKey] = profileEntry.PublicKey
		}
	}

	// Sigh.. convert the public keys *back* into PKIDs...
	profilesFound := []*ProfileEntry{}
	for _, pk := range pubKeysMap {
		pkid := utxoView.GetPKIDForPublicKey(pk).PKID
		profile := utxoView.GetProfileEntryForPKID(pkid)
		// Double-check that a username matches the prefix.
		// If a user had the handle "elon" and then changed to "jeff" and that transaction hadn't mined yet,
		// we would return the profile for "jeff" when we search for "elon" which is incorrect.
		if profile != nil && strings.HasPrefix(strings.ToLower(string(profile.Username[:])), lowercaseUsernamePrefixString) {
			profilesFound = append(profilesFound, profile)
		}
	}

	// If there is no error, sort and return numToFetch. Username searches are always
	// sorted by coin value.
	sort.Slice(profilesFound, func(ii, jj int) bool {
		return profilesFound[ii].CoinEntry.BitCloutLockedNanos > profilesFound[jj].CoinEntry.BitCloutLockedNanos
	})

	return profilesFound, nil
}

// DBGetPaginatedProfilesByBitCloutLocked returns up to 'numToFetch' profiles from the db.
func DBGetPaginatedProfilesByBitCloutLocked(
	db *badger.DB, startBitCloutLockedNanos uint64,
	startProfilePubKeyy []byte, numToFetch int, fetchProfileEntries bool) (
	_profilePublicKeys [][]byte, _profileEntries []*ProfileEntry, _err error) {

	// Convert the start public key to a PKID.
	pkidEntry := DBGetPKIDEntryForPublicKey(db, startProfilePubKeyy)

	startProfilePrefix := append([]byte{}, _PrefixCreatorBitCloutLockedNanosCreatorPKID...)
	var startBitCloutLockedBytes []byte
	if pkidEntry != nil {
		startBitCloutLockedBytes = EncodeUint64(startBitCloutLockedNanos)
		startProfilePrefix = append(startProfilePrefix, startBitCloutLockedBytes...)
		startProfilePrefix = append(startProfilePrefix, pkidEntry.PKID[:]...)
	} else {
		// If no pub key is provided, we just max out bitclout locked and start at the top of the list.
		maxBigEndianUint64Bytes := []byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}
		startBitCloutLockedBytes = maxBigEndianUint64Bytes
		startProfilePrefix = append(startProfilePrefix, startBitCloutLockedBytes...)
	}

	keyLen := len(_PrefixCreatorBitCloutLockedNanosCreatorPKID) + len(startBitCloutLockedBytes) + btcec.PubKeyBytesLenCompressed
	// We fetch in reverse to get the profiles with the most BitClout locked.
	profileIndexKeys, _, err := DBGetPaginatedKeysAndValuesForPrefix(
		db, startProfilePrefix, _PrefixCreatorBitCloutLockedNanosCreatorPKID, /*validForPrefix*/
		keyLen /*keyLen*/, numToFetch,
		true /*reverse*/, false /*fetchValues*/)
	if err != nil {
		return nil, nil, fmt.Errorf("DBGetPaginatedProfilesByBitCloutLocked: %v", err)
	}

	// Cut the pkids out of the returned keys.
	profilePKIDs := [][]byte{}
	startPKIDIndex := len(_PrefixCreatorBitCloutLockedNanosCreatorPKID) + len(startBitCloutLockedBytes)
	endPKIDIndex := startPKIDIndex + btcec.PubKeyBytesLenCompressed
	for _, profileKeyBytes := range profileIndexKeys {
		currentPKID := make([]byte, btcec.PubKeyBytesLenCompressed)
		copy(currentPKID[:], profileKeyBytes[startPKIDIndex:endPKIDIndex][:])
		profilePKIDs = append(profilePKIDs, currentPKID)
	}

	profilePubKeys := [][]byte{}
	for _, pkidBytes := range profilePKIDs {
		pkid := &PKID{}
		copy(pkid[:], pkidBytes)
		profilePubKeys = append(profilePubKeys, DBGetPublicKeyForPKID(db, pkid))
	}

	if !fetchProfileEntries {
		return profilePubKeys, nil, nil
	}

	// Fetch the ProfileEntries if desired.
	var profileEntries []*ProfileEntry
	for _, profilePKID := range profilePKIDs {
		pkid := &PKID{}
		copy(pkid[:], profilePKID)
		profileEntry := DBGetProfileEntryForPKID(db, pkid)
		if profileEntry == nil {
			return nil, nil, fmt.Errorf("DBGetAllProfilesByLockedBitClout: "+
				"ProfilePKID %v does not have corresponding entry",
				PkToStringBoth(profilePKID))
		}
		profileEntries = append(profileEntries, profileEntry)
	}

	return profilePubKeys, profileEntries, nil
}

// -------------------------------------------------------------------------------------
// Mempool Txn mapping funcions
// <prefix, txn hash BlockHash> -> <*MsgBitCloutTxn>
// -------------------------------------------------------------------------------------

func _dbKeyForMempoolTxn(mempoolTx *MempoolTx) []byte {
	// Make a copy to avoid multiple calls to this function re-using the same slice.
	prefixCopy := append([]byte{}, _PrefixMempoolTxnHashToMsgBitCloutTxn...)
	timeAddedBytes := EncodeUint64(uint64(mempoolTx.Added.UnixNano()))
	key := append(prefixCopy, timeAddedBytes...)
	key = append(key, mempoolTx.Hash[:]...)

	return key
}

func DbPutMempoolTxnWithTxn(txn *badger.Txn, mempoolTx *MempoolTx) error {

	mempoolTxnBytes, err := mempoolTx.Tx.ToBytes(false /*preSignatureBool*/)
	if err != nil {
		return errors.Wrapf(err, "DbPutMempoolTxnWithTxn: Problem encoding mempoolTxn to bytes.")
	}

	if err := txn.Set(_dbKeyForMempoolTxn(mempoolTx), mempoolTxnBytes); err != nil {
		return errors.Wrapf(err, "DbPutMempoolTxnWithTxn: Problem putting mapping for txn hash: %s", mempoolTx.Hash.String())
	}

	return nil
}

func DbPutMempoolTxn(handle *badger.DB, mempoolTx *MempoolTx) error {

	return handle.Update(func(txn *badger.Txn) error {
		return DbPutMempoolTxnWithTxn(txn, mempoolTx)
	})
}

func DbGetMempoolTxnWithTxn(txn *badger.Txn, mempoolTx *MempoolTx) *MsgBitCloutTxn {

	mempoolTxnObj := &MsgBitCloutTxn{}
	mempoolTxnItem, err := txn.Get(_dbKeyForMempoolTxn(mempoolTx))
	if err != nil {
		return nil
	}
	err = mempoolTxnItem.Value(func(valBytes []byte) error {
		return gob.NewDecoder(bytes.NewReader(valBytes)).Decode(mempoolTxnObj)
	})
	if err != nil {
		glog.Errorf("DbGetMempoolTxnWithTxn: Problem reading "+
			"Tx for tx hash %s: %v", mempoolTx.Hash.String(), err)
		return nil
	}
	return mempoolTxnObj
}

func DbGetMempoolTxn(db *badger.DB, mempoolTx *MempoolTx) *MsgBitCloutTxn {
	var ret *MsgBitCloutTxn
	db.View(func(txn *badger.Txn) error {
		ret = DbGetMempoolTxnWithTxn(txn, mempoolTx)
		return nil
	})
	return ret
}

func DbGetAllMempoolTxnsSortedByTimeAdded(handle *badger.DB) (_mempoolTxns []*MsgBitCloutTxn, _error error) {
	_, valuesFound := _enumerateKeysForPrefix(handle, _PrefixMempoolTxnHashToMsgBitCloutTxn)

	mempoolTxns := []*MsgBitCloutTxn{}
	for _, mempoolTxnBytes := range valuesFound {
		mempoolTxn := &MsgBitCloutTxn{}
		err := mempoolTxn.FromBytes(mempoolTxnBytes)
		if err != nil {
			return nil, errors.Wrapf(err, "DbGetAllMempoolTxnsSortedByTimeAdded: failed to decode mempoolTxnBytes.")
		}
		mempoolTxns = append(mempoolTxns, mempoolTxn)
	}

	// We don't need to sort the transactions because the DB keys include the time added and
	// are therefore retrieved from badger in order.

	return mempoolTxns, nil
}

func DbDeleteAllMempoolTxnsWithTxn(txn *badger.Txn) error {
	txnKeysFound, _, err := _enumerateKeysForPrefixWithTxn(txn, _PrefixMempoolTxnHashToMsgBitCloutTxn)
	if err != nil {
		return errors.Wrapf(err, "DbDeleteAllMempoolTxnsWithTxn: ")
	}

	for _, txnKey := range txnKeysFound {
		err := DbDeleteMempoolTxnKeyWithTxn(txn, txnKey)
		if err != nil {
			return errors.Wrapf(err, "DbDeleteAllMempoolTxMappings: Deleting mempool txnKey failed.")
		}
	}

	return nil
}

func FlushMempoolToDbWithTxn(txn *badger.Txn, allTxns []*MempoolTx) error {
	for _, mempoolTx := range allTxns {
		err := DbPutMempoolTxnWithTxn(txn, mempoolTx)
		if err != nil {
			return errors.Wrapf(err, "FlushMempoolToDb: Putting "+
				"mempool tx hash %s failed.", mempoolTx.Hash.String())
		}
	}

	return nil
}

func FlushMempoolToDb(handle *badger.DB, allTxns []*MempoolTx) error {
	err := handle.Update(func(txn *badger.Txn) error {
		return FlushMempoolToDbWithTxn(txn, allTxns)
	})
	if err != nil {
		return err
	}

	return nil
}

func DbDeleteAllMempoolTxns(handle *badger.DB) error {
	handle.Update(func(txn *badger.Txn) error {
		return DbDeleteAllMempoolTxnsWithTxn(txn)
	})

	return nil
}

func DbDeleteMempoolTxnWithTxn(txn *badger.Txn, mempoolTx *MempoolTx) error {

	// When a mapping exists, delete it.
	if err := txn.Delete(_dbKeyForMempoolTxn(mempoolTx)); err != nil {
		return errors.Wrapf(err, "DbDeleteMempoolTxMappingWithTxn: Deleting "+
			"mempool tx key failed.")
	}

	return nil
}

func DbDeleteMempoolTxn(handle *badger.DB, mempoolTx *MempoolTx) error {
	return handle.Update(func(txn *badger.Txn) error {
		return DbDeleteMempoolTxnWithTxn(txn, mempoolTx)
	})
}

func DbDeleteMempoolTxnKey(handle *badger.DB, txnKey []byte) error {
	return handle.Update(func(txn *badger.Txn) error {
		return DbDeleteMempoolTxnKeyWithTxn(txn, txnKey)
	})
}

func DbDeleteMempoolTxnKeyWithTxn(txn *badger.Txn, txnKey []byte) error {

	// When a mapping exists, delete it.
	if err := txn.Delete(txnKey); err != nil {
		return errors.Wrapf(err, "DbDeleteMempoolTxMappingWithTxn: Deleting "+
			"mempool tx key failed.")
	}

	return nil
}

func LogDBSummarySnapshot(db *badger.DB) {
	keyCountMap := make(map[byte]int)
	for prefixByte := byte(0); prefixByte < byte(40); prefixByte++ {
		keysForPrefix, _ := EnumerateKeysForPrefix(db, []byte{prefixByte})
		keyCountMap[prefixByte] = len(keysForPrefix)
	}
	glog.Info(spew.Printf("LogDBSummarySnapshot: Current DB summary snapshot: %v", keyCountMap))
}

func StartDBSummarySnapshots(db *badger.DB) {
	// Periodically count the number of keys for each prefix in the DB and log.
	go func() {
		for {
			// Figure out how many keys there are for each prefix and log.
			glog.Info("StartDBSummarySnapshots: Counting DB keys...")
			LogDBSummarySnapshot(db)
			time.Sleep(30 * time.Second)
		}
	}()
}
