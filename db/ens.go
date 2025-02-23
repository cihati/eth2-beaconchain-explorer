package db

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"eth2-exporter/ens"
	"eth2-exporter/types"
	"eth2-exporter/utils"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	gcp_bigtable "cloud.google.com/go/bigtable"
	"golang.org/x/sync/errgroup"

	"github.com/coocood/freecache"
	"github.com/ethereum/go-ethereum/common"
	eth_types "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"

	go_ens "github.com/wealdtech/go-ens/v3"
)

// https://etherscan.io/tx/0x9fec76750a504e5610643d1882e3b07f4fc786acf7b9e6680697bb7165de1165#eventlog
// TransformEnsNameRegistered accepts an eth1 block and creates bigtable mutations for ENS Name events.
// It transforms the logs contained within a block and indexes ens relevant transactions and tags changes (to be verified from the node in a separate process)
// ==================================================
//
// It indexes transactions
//
// - by hashed ens name
// Row:    <chainID>:ENS:I:H:<nameHash>:<txHash>
// Family: f
// Column: nil
// Cell:   nil
// Example scan: "5:ENS:I:H:4ae569dd0aa2f6e9207e41423c956d0d27cbc376a499ee8d90fe1d84489ae9d1:e627ae94bd16eb1ed8774cd4003fc25625159f13f8a2612cc1c7f8d2ab11b1d7"
//
// - by address
// Row:    <chainID>:ENS:I:A:<address>:<txHash>
// Family: f
// Column: nil
// Cell:   nil
// Example scan: "5:ENS:I:A:05579fadcf7cc6544f7aa018a2726c85251600c5:e627ae94bd16eb1ed8774cd4003fc25625159f13f8a2612cc1c7f8d2ab11b1d7"
//
// ==================================================
//
// Track for later verification via the node ("set dirty")
//
// - by name
// Row:    <chainID>:ENS:V:N:<name>
// Family: f
// Column: nil
// Cell:   nil
// Example scan: "5:ENS:V:N:somename"
//
// - by name hash
// Row:    <chainID>:ENS:V:H:<nameHash>
// Family: f
// Column: nil
// Cell:   nil
// Example scan: "5:ENS:V:H:6f5d9cc23e60abe836401b4fd386ec9280a1f671d47d9bf3ec75dab76380d845"
//
// - by address
// Row:    <chainID>:ENS:V:A:<address>
// Family: f
// Column: nil
// Cell:   nil
// Example scan: "5:ENS:V:A:27234cb8734d5b1fac0521c6f5dc5aebc6e839b6"
//
// ==================================================

func (bigtable *Bigtable) TransformEnsNameRegistered(blk *types.Eth1Block, cache *freecache.Cache) (bulkData *types.BulkMutations, bulkMetadataUpdates *types.BulkMutations, err error) {
	bulkData = &types.BulkMutations{}
	bulkMetadataUpdates = &types.BulkMutations{}

	filterer, err := ens.NewEnsRegistrarFilterer(common.Address{}, nil)
	if err != nil {
		log.Printf("error creating filterer: %v", err)
		return nil, nil, err
	}
	keys := make(map[string]bool)

	for i, tx := range blk.GetTransactions() {
		if i > 9999 {
			return nil, nil, fmt.Errorf("unexpected number of transactions in block expected at most 9999 but got: %v, tx: %x", i, tx.GetHash())
		}

		// We look for the different ENS events,
		// 	most will be triggered by a main registrar contract,
		//  but some are triggered on a different contracts (like a resolver contract), these will be validated when loading the related events
		var isRegistarContract = len(utils.Config.Indexer.EnsTransformer.ValidRegistrarContracts) > 0 && utils.SliceContains(utils.Config.Indexer.EnsTransformer.ValidRegistrarContracts, common.BytesToAddress(tx.To).String())
		foundNameIndex := -1
		foundResolverIndex := -1
		foundNameRenewedIndex := -1
		foundAddressChangedIndices := []int{}
		foundNameChangedIndex := -1
		foundNewOwnerIndex := -1
		logs := tx.GetLogs()
		for j, log := range logs {
			if j > 99999 {
				return nil, nil, fmt.Errorf("unexpected number of logs in block expected at most 99999 but got: %v tx: %x", j, tx.GetHash())
			}
			for _, lTopic := range log.GetTopics() {
				if isRegistarContract {
					if bytes.Equal(lTopic, ens.NameRegisteredTopic) {
						foundNameIndex = j
					} else if bytes.Equal(lTopic, ens.NewResolverTopic) {
						foundResolverIndex = j
					} else if bytes.Equal(lTopic, ens.NameRenewedTopic) {
						foundNameRenewedIndex = j
					}
				} else if bytes.Equal(lTopic, ens.AddressChangedTopic) {
					foundAddressChangedIndices = append(foundAddressChangedIndices, j)
				} else if bytes.Equal(lTopic, ens.NameChangedTopic) {
					foundNameChangedIndex = j
				} else if bytes.Equal(lTopic, ens.NewOwnerTopic) {
					foundNewOwnerIndex = j
				}
			}
		}
		// We found a register name event
		if foundNameIndex > -1 && foundResolverIndex > -1 {

			log := logs[foundNameIndex]
			topics := make([]common.Hash, 0, len(log.GetTopics()))

			for _, lTopic := range log.GetTopics() {
				topics = append(topics, common.BytesToHash(lTopic))
			}

			nameLog := eth_types.Log{
				Address:     common.BytesToAddress(log.GetAddress()),
				Data:        log.Data,
				Topics:      topics,
				BlockNumber: blk.GetNumber(),
				TxHash:      common.BytesToHash(tx.GetHash()),
				TxIndex:     uint(i),
				BlockHash:   common.BytesToHash(blk.GetHash()),
				Index:       uint(foundNameIndex),
				Removed:     log.GetRemoved(),
			}

			log = logs[foundResolverIndex]
			topics = make([]common.Hash, 0, len(log.GetTopics()))

			for _, lTopic := range log.GetTopics() {
				topics = append(topics, common.BytesToHash(lTopic))
			}

			resolverLog := eth_types.Log{
				Address:     common.BytesToAddress(log.GetAddress()),
				Data:        log.Data,
				Topics:      topics,
				BlockNumber: blk.GetNumber(),
				TxHash:      common.BytesToHash(tx.GetHash()),
				TxIndex:     uint(i),
				BlockHash:   common.BytesToHash(blk.GetHash()),
				Index:       uint(foundResolverIndex),
				Removed:     log.GetRemoved(),
			}

			nameRegistered, err := filterer.ParseNameRegistered(nameLog)
			if err != nil {
				utils.LogError(err, "indexing of register event failed parse register event", 0)
				continue
			}
			resolver, err := filterer.ParseNewResolver(resolverLog)
			if err != nil {
				utils.LogError(err, "indexing of register event failed parse resolver event", 0)
				continue
			}

			keys[fmt.Sprintf("%s:ENS:I:H:%x:%x", bigtable.chainId, resolver.Node, tx.GetHash())] = true
			keys[fmt.Sprintf("%s:ENS:I:A:%x:%x", bigtable.chainId, nameRegistered.Owner, tx.GetHash())] = true
			keys[fmt.Sprintf("%s:ENS:V:A:%x", bigtable.chainId, nameRegistered.Owner)] = true
			keys[fmt.Sprintf("%s:ENS:V:N:%s", bigtable.chainId, nameRegistered.Name)] = true

		} else if foundNameRenewedIndex > -1 { // We found a renew name event
			log := logs[foundNameRenewedIndex]
			topics := make([]common.Hash, 0, len(log.GetTopics()))

			for _, lTopic := range log.GetTopics() {
				topics = append(topics, common.BytesToHash(lTopic))
			}

			nameRenewedLog := eth_types.Log{
				Address:     common.BytesToAddress(log.GetAddress()),
				Data:        log.Data,
				Topics:      topics,
				BlockNumber: blk.GetNumber(),
				TxHash:      common.BytesToHash(tx.GetHash()),
				TxIndex:     uint(i),
				BlockHash:   common.BytesToHash(blk.GetHash()),
				Index:       uint(foundNameRenewedIndex),
				Removed:     log.GetRemoved(),
			}

			nameRenewed, err := filterer.ParseNameRenewed(nameRenewedLog)
			if err != nil {
				utils.LogError(err, "indexing of renew event failed parse event", 0)
				continue
			}

			nameHash, err := go_ens.NameHash(nameRenewed.Name)
			if err != nil {
				utils.LogError(err, "error hashing ens name", 0)
				continue
			}
			keys[fmt.Sprintf("%s:ENS:I:H:%x:%x", bigtable.chainId, nameHash, tx.GetHash())] = true
			keys[fmt.Sprintf("%s:ENS:V:N:%s", bigtable.chainId, nameRenewed.Name)] = true

		} else if foundNameChangedIndex > -1 && foundNewOwnerIndex > -1 { // we found a name change event

			log := logs[foundNewOwnerIndex]
			topics := make([]common.Hash, 0, len(log.GetTopics()))

			for _, lTopic := range log.GetTopics() {
				topics = append(topics, common.BytesToHash(lTopic))
			}
			newOwnerLog := eth_types.Log{
				Address:     common.BytesToAddress(log.GetAddress()),
				Data:        log.Data,
				Topics:      topics,
				BlockNumber: blk.GetNumber(),
				TxHash:      common.BytesToHash(tx.GetHash()),
				TxIndex:     uint(i),
				BlockHash:   common.BytesToHash(blk.GetHash()),
				Index:       uint(foundNewOwnerIndex),
				Removed:     log.GetRemoved(),
			}

			newOwner, err := filterer.ParseNewOwner(newOwnerLog)
			if err != nil {
				utils.LogError(err, fmt.Errorf("indexing of new owner event failed parse event at index %v", foundNewOwnerIndex), 0)
				continue
			}

			keys[fmt.Sprintf("%s:ENS:I:A:%x:%x", bigtable.chainId, newOwner.Owner, tx.GetHash())] = true
			keys[fmt.Sprintf("%s:ENS:V:A:%x", bigtable.chainId, newOwner.Owner)] = true
		}
		// We found a change address event, there can be multiple within one transaction
		for _, addressChangeIndex := range foundAddressChangedIndices {

			log := logs[addressChangeIndex]
			topics := make([]common.Hash, 0, len(log.GetTopics()))

			for _, lTopic := range log.GetTopics() {
				topics = append(topics, common.BytesToHash(lTopic))
			}

			addressChangedLog := eth_types.Log{
				Address:     common.BytesToAddress(log.GetAddress()),
				Data:        log.Data,
				Topics:      topics,
				BlockNumber: blk.GetNumber(),
				TxHash:      common.BytesToHash(tx.GetHash()),
				TxIndex:     uint(i),
				BlockHash:   common.BytesToHash(blk.GetHash()),
				Index:       uint(addressChangeIndex),
				Removed:     log.GetRemoved(),
			}

			addressChanged, err := filterer.ParseAddressChanged(addressChangedLog)
			if err != nil {
				utils.LogError(err, "indexing of address change event failed parse event at index ", 0)
				continue
			}

			keys[fmt.Sprintf("%s:ENS:I:H:%x:%x", bigtable.chainId, addressChanged.Node, tx.GetHash())] = true
			keys[fmt.Sprintf("%s:ENS:V:H:%x", bigtable.chainId, addressChanged.Node)] = true

		}
	}
	for key := range keys {
		mut := gcp_bigtable.NewMutation()
		mut.Set(DEFAULT_FAMILY, key, gcp_bigtable.Timestamp(0), nil)

		bulkData.Keys = append(bulkData.Keys, key)
		bulkData.Muts = append(bulkData.Muts, mut)
	}

	return bulkData, bulkMetadataUpdates, nil
}

type EnsCheckedDictionary struct {
	mux     sync.Mutex
	address map[common.Address]bool
	name    map[string]bool
}

func (bigtable *Bigtable) ImportEnsUpdates(client *ethclient.Client) error {
	key := fmt.Sprintf("%s:ENS:V", bigtable.chainId)

	ctx, done := context.WithTimeout(context.Background(), time.Second*30)
	defer done()

	rowRange := gcp_bigtable.PrefixRange(key)
	keys := []string{}

	err := bigtable.tableData.ReadRows(ctx, rowRange, func(row gcp_bigtable.Row) bool {
		row_ := row[DEFAULT_FAMILY][0]
		keys = append(keys, row_.Row)
		return true
	})
	if err != nil {
		return err
	}

	if len(keys) == 0 {
		logger.Info("No ENS entries to validate")
		return nil
	}

	logger.Infof("Validating %v ENS entries", len(keys))
	alreadyChecked := EnsCheckedDictionary{
		address: make(map[common.Address]bool),
		name:    make(map[string]bool),
	}
	mutsDelete := &types.BulkMutations{
		Keys: make([]string, 0, 1),
		Muts: make([]*gcp_bigtable.Mutation, 0, 1),
	}

	batchSize := 100
	total := len(keys)
	for i := 0; i < total; i += batchSize {
		to := i + batchSize
		if to > total {
			to = total
		}
		batch := keys[i:to]
		logger.Infof("Batching ENS entries %v:%v of %v", i, to, total)
		g := new(errgroup.Group)
		mutDelete := gcp_bigtable.NewMutation()
		mutDelete.DeleteRow()
		for _, k := range batch {
			key := k
			var name string
			var address *common.Address
			split := strings.Split(key, ":")
			value := split[4]
			switch split[3] {
			case "H":
				// if we have a hash we look if we find a name in the db. If not we can ignore it.
				nameHash, err := hex.DecodeString(value)
				if err != nil {
					utils.LogError(err, fmt.Errorf("name hash could not be decoded: %v", value), 0)
				} else {
					err := ReaderDb.Get(&name, `
					SELECT
						ens_name
					FROM ens
					WHERE name_hash = $1
					`, nameHash[:])
					if err != nil && err != sql.ErrNoRows {
						return err
					}
				}
			case "A":
				addressHash, err := hex.DecodeString(value)
				if err != nil {
					utils.LogError(err, fmt.Errorf("address hash could not be decoded: %v", value), 0)
				} else {
					add := common.BytesToAddress(addressHash)
					address = &add
				}
			case "N":
				name = value
			}

			mutsDelete.Keys = append(mutsDelete.Keys, key)
			mutsDelete.Muts = append(mutsDelete.Muts, mutDelete)

			g.Go(func() error {
				if name != "" {
					err := validateEnsName(client, name, &alreadyChecked, nil)
					if err != nil {
						return err
					}
				} else if address != nil {
					err := validateEnsAddress(client, *address, &alreadyChecked)
					if err != nil {
						return err
					}
				}
				return nil
			})
		}
		if err := g.Wait(); err != nil {
			return err
		}
	}
	logger.Info("ens key indexing completed")
	// After processing the keys we remove them from bigtable
	return bigtable.WriteBulk(mutsDelete, bigtable.tableData)
}

func validateEnsAddress(client *ethclient.Client, address common.Address, alreadyChecked *EnsCheckedDictionary) error {

	alreadyChecked.mux.Lock()
	if alreadyChecked.address[address] {
		alreadyChecked.mux.Unlock()
		return nil
	}
	alreadyChecked.address[address] = true
	alreadyChecked.mux.Unlock()

	name, err := go_ens.ReverseResolve(client, address)
	if err != nil {
		utils.LogError(err, fmt.Errorf("address could not be reverse resolved: %v", address), 0)
		return removeEnsAddress(client, address, alreadyChecked)
	}

	currentName, err := GetEnsNameForAddress(address)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	isPrimary := false
	if currentName != nil {
		if *currentName == name {
			return nil
		}
		logger.Infof("Address [%x] has a new main name from %x to: %v", address, *currentName, name)
		err := validateEnsName(client, *currentName, alreadyChecked, &isPrimary)
		if err != nil {
			return err
		}
	}
	isPrimary = true
	logger.Infof("Address [%x] has a primary name: %v", address, name)
	return validateEnsName(client, name, alreadyChecked, &isPrimary)
}

func validateEnsName(client *ethclient.Client, name string, alreadyChecked *EnsCheckedDictionary, isPrimaryName *bool) error {
	// For now only .eth is supported other ens domains use different techniques and require and individual implementation
	if !strings.HasSuffix(name, ".eth") {
		name = fmt.Sprintf("%s.eth", name)
	}
	alreadyChecked.mux.Lock()
	if alreadyChecked.name[name] {
		alreadyChecked.mux.Unlock()
		return nil
	}
	alreadyChecked.name[name] = true
	alreadyChecked.mux.Unlock()

	nameHash, err := go_ens.NameHash(name)
	if err != nil {
		utils.LogError(err, fmt.Errorf("could not hash name: %v", name), 0)
		return nil
	}

	addr, err := go_ens.Resolve(client, name)
	if err != nil {
		utils.LogError(err, fmt.Errorf("error resolving name: %v", name), 0)
		return removeEnsName(client, name)
	}
	ensName, err := go_ens.NewName(client, name)
	if err != nil {
		utils.LogError(err, fmt.Errorf("error getting create ens name: %v", name), 0)
		return removeEnsName(client, name)
	}
	expires, err := ensName.Expires()
	if err != nil {
		utils.LogError(err, fmt.Errorf("error get ens expire date: %v", name), 0)
		return removeEnsName(client, name)
	}
	isPrimary := false
	if isPrimaryName == nil {
		reverseName, err := go_ens.ReverseResolve(client, addr)
		if err == nil && reverseName == name {
			isPrimary = true
		}
	} else if *isPrimaryName {
		isPrimary = true
	}
	_, err = WriterDb.Exec(`
	INSERT INTO ens (
		name_hash, 
		ens_name, 
		address,
		is_primary_name, 
		valid_to)
	VALUES ($1, $2, $3, $4, $5) 
	ON CONFLICT 
		(name_hash) 
	DO UPDATE SET 
		ens_name = excluded.ens_name,
		address = excluded.address,
		is_primary_name = excluded.is_primary_name,
		valid_to = excluded.valid_to
	`, nameHash[:], name, addr.Bytes(), isPrimary, expires)
	if err != nil {
		utils.LogError(err, fmt.Errorf("error writing ens data for name [%v]", name), 0)
		return err
	}
	logger.Infof("Name [%v] resolved -> %x, expires: %v, is primary: %v", name, addr, expires, isPrimary)
	return nil
}

func removeEnsAddress(client *ethclient.Client, address common.Address, alreadyChecked *EnsCheckedDictionary) error {
	name, err := GetEnsNameForAddress(address)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if name == nil {
		return nil
	}
	isPrimary := false
	return validateEnsName(client, *name, alreadyChecked, &isPrimary)
}

func removeEnsName(client *ethclient.Client, name string) error {
	_, err := WriterDb.Exec(`
	DELETE FROM ens 
	WHERE 
		ens_name = $1
	;`, name)
	if err != nil {
		utils.LogError(err, fmt.Errorf("error deleting ens name [%v]", name), 0)
		return err
	}
	logger.Infof("Ens name remove from db: %v", name)
	return nil
}

func GetAddressForEnsName(name string) (address *common.Address, err error) {
	addressBytes := []byte{}
	err = ReaderDb.Get(&addressBytes, `
	SELECT address 
	FROM ens
	WHERE
		ens_name = $1 AND
		valid_to >= now()
	`, name)
	if err == nil && addressBytes != nil {
		add := common.BytesToAddress(addressBytes)
		address = &add
	}
	return address, err
}

func GetEnsNameForAddress(address common.Address) (name *string, err error) {
	err = ReaderDb.Get(&name, `
	SELECT ens_name 
	FROM ens
	WHERE
		address = $1 AND
		is_primary_name AND
		valid_to >= now()
	;`, address.Bytes())
	return name, err
}
