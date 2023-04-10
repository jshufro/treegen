package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/fatih/color"
	"github.com/rocket-pool/rocketpool-go/rewards"
	"github.com/rocket-pool/rocketpool-go/rocketpool"
	"github.com/rocket-pool/rocketpool-go/utils/eth"
	"github.com/rocket-pool/smartnode/shared/services/beacon"
	"github.com/rocket-pool/smartnode/shared/services/beacon/client"
	"github.com/rocket-pool/smartnode/shared/services/config"
	rprewards "github.com/rocket-pool/smartnode/shared/services/rewards"
	"github.com/rocket-pool/smartnode/shared/services/state"
	cfgtypes "github.com/rocket-pool/smartnode/shared/types/config"
	"github.com/rocket-pool/smartnode/shared/utils/log"
	"github.com/urfave/cli/v2"
)

const (
	MaxConcurrentEth1Requests = 200
)

type snapshotDetails struct {
	index                 uint64
	startTime             time.Time
	endTime               time.Time
	snapshotBeaconBlock   uint64
	snapshotElBlockHeader *types.Header
	intervalsPassed       uint64
}

type treeGenerator struct {
	log          *log.ColorLogger
	rp           *rocketpool.RocketPool
	cfg          *config.RocketPoolConfig
	bn           beacon.Client
	beaconConfig beacon.Eth2Config

	outputDir   string
	prettyPrint bool
	ruleset     uint64
}

func GenerateTree(c *cli.Context) error {

	// Configure
	configureHTTP()

	// Initialization
	currentIndex := c.Int64("interval")
	targetEpoch := c.Uint64("target-epoch")
	log := log.NewColorLogger(color.FgHiWhite)

	// URL acquisiton
	ecUrl := c.String("ec-endpoint")
	if ecUrl == "" {
		return fmt.Errorf("ec-endpoint must be provided")
	}
	bnUrl := c.String("bn-endpoint")
	if ecUrl == "" {
		return fmt.Errorf("bn-endpoint must be provided")
	}

	// Create the EC and BN clients
	ec, err := ethclient.Dial(ecUrl)
	if err != nil {
		return fmt.Errorf("error connecting to the EC: %w", err)
	}
	bn := client.NewStandardHttpClient(bnUrl)
	beaconConfig, err := bn.GetEth2Config()
	if err != nil {
		return fmt.Errorf("error getting beacon config from the bn at %s - %w", bnUrl, err)
	}

	// Check which network we're on via the BN
	depositContract, err := bn.GetEth2DepositContract()
	if err != nil {
		return fmt.Errorf("error getting deposit contract from the BN: %w", err)
	}
	var network cfgtypes.Network
	switch depositContract.ChainID {
	case 1:
		network = cfgtypes.Network_Mainnet
		log.Printlnf("Beacon node is configured for Mainnet.")
	case 5:
		network = cfgtypes.Network_Prater
		log.Printlnf("Beacon node is configured for Prater.")
		/*
			case 1337803:
				network = cfgtypes.Network_Zhejiang
				log.Printlnf("Beacon node is configured for Zhejiang.")*/
	default:
		return fmt.Errorf("your Beacon node is configured for an unknown network with Chain ID [%d]", depositContract.ChainID)
	}

	// Create a new config on the proper network
	cfg := config.NewRocketPoolConfig("", true)
	cfg.Smartnode.Network.Value = network

	// Create the RP wrapper
	storageContract := cfg.Smartnode.GetStorageAddress()
	rp, err := rocketpool.NewRocketPool(ec, common.HexToAddress(storageContract))
	if err != nil {
		return fmt.Errorf("error creating Rocket Pool wrapper: %w", err)
	}

	// Create the generator
	generator := treeGenerator{
		log:          &log,
		rp:           rp,
		cfg:          cfg,
		bn:           bn,
		beaconConfig: beaconConfig,
		outputDir:    c.String("output-dir"),
		prettyPrint:  c.Bool("pretty-print"),
		ruleset:      c.Uint64("ruleset"),
	}

	// Print the network info and exit if requested
	if c.Bool("network-info") {
		return generator.printNetworkInfo(nil)
	}

	// Run the tree generation or the rETH SP approximation
	if c.Bool("approximate-only") {
		return generator.approximateCurrentRethSpRewards()
	}

	var targetBlock uint64
	if targetEpoch > 0 {
		targetBlock, err = generator.lastBlockInEpoch(targetEpoch)
		if err != nil {
			return err
		}
	}

	if c.Bool("validator-stats") {
		return generator.validatorStats(targetEpoch)
	}

	if currentIndex < 0 {
		return generator.generatePartialTree(targetBlock)
	}

	return generator.generatePastTree(uint64(currentIndex), targetBlock)
}

func (g *treeGenerator) lastBlockInEpoch(epoch uint64) (uint64, error) {
	end := epoch * g.beaconConfig.SlotsPerEpoch
	start := end + g.beaconConfig.SlotsPerEpoch - 1
	for block := start; block >= end; block-- {
		_, exists, err := g.bn.GetBeaconBlock(fmt.Sprint(block))
		if err != nil {
			return 0, err
		}

		if exists {
			return block, nil
		}

		g.log.Printlnf("No proposal in epoch %d at slot %d...", epoch, block-end)
	}

	return 0, fmt.Errorf("Epoch %d appears to have had no blocks proposed, or all are missing from the bn", epoch)
}

func (g *treeGenerator) generateRewardsFile(treegen *rprewards.TreeGenerator) (*rprewards.RewardsFile, error) {
	if g.ruleset == 0 {
		return treegen.GenerateTree()
	}

	return treegen.GenerateTreeWithRuleset(g.ruleset)
}

func (g *treeGenerator) serializeMinipoolPerformance(rewardsFile *rprewards.RewardsFile) ([]byte, error) {
	if g.prettyPrint {
		return json.MarshalIndent(rewardsFile.MinipoolPerformanceFile, "", "\t")
	}

	return json.Marshal(rewardsFile.MinipoolPerformanceFile)
}

func (g *treeGenerator) serializeRewardsTree(rewardsFile *rprewards.RewardsFile) ([]byte, error) {
	if g.prettyPrint {
		return json.MarshalIndent(rewardsFile, "", "\t")
	}

	return json.Marshal(rewardsFile)
}

func (g *treeGenerator) getState(rewardsEvent *rewards.RewardsEvent) (*state.NetworkState, error) {
	var slot uint64

	// Get a snapshot of the network state
	mgr, err := state.NewNetworkStateManager(g.rp, g.cfg, g.rp.Client, g.bn, g.log)
	if err != nil {
		return nil, fmt.Errorf("error creating network state manager: %w", err)
	}

	if rewardsEvent == nil {
		block, err := mgr.GetLatestFinalizedBeaconBlock()
		if err != nil {
			return nil, fmt.Errorf("error getting latest finalized Beacon block: %w", err)
		}

		slot = block.Slot
	} else {
		slot = rewardsEvent.ConsensusBlock.Uint64()
	}

	state, err := mgr.GetStateForSlot(slot)
	if err != nil {
		return nil, fmt.Errorf("error getting network state: %w", err)
	}

	return state, nil
}

// Writes both the performance file and the rewards file to disk
func (g *treeGenerator) writeFiles(rewardsFile *rprewards.RewardsFile, index uint64) error {
	g.log.Printlnf("Saving JSON files...")

	// Get the output paths
	rewardsTreePath := filepath.Join(g.outputDir, fmt.Sprintf(config.RewardsTreeFilenameFormat, string(g.cfg.Smartnode.Network.Value.(cfgtypes.Network)), index))
	minipoolPerformancePath := filepath.Join(g.outputDir, fmt.Sprintf(config.MinipoolPerformanceFilenameFormat, string(g.cfg.Smartnode.Network.Value.(cfgtypes.Network)), index))

	// Serialize the minipool performance file
	minipoolPerformanceBytes, err := g.serializeMinipoolPerformance(rewardsFile)
	if err != nil {
		return fmt.Errorf("error serializing minipool performance file into JSON: %w", err)
	}

	// Write it to disk
	err = os.WriteFile(minipoolPerformancePath, minipoolPerformanceBytes, 0644)
	if err != nil {
		return fmt.Errorf("error saving minipool performance file to %s: %w", minipoolPerformancePath, err)
	}

	g.log.Printlnf("Saved minipool performance file to %s", minipoolPerformancePath)
	rewardsFile.MinipoolPerformanceFileCID = "---"

	// Serialize the rewards tree to JSON
	wrapperBytes, err := g.serializeRewardsTree(rewardsFile)
	if err != nil {
		return fmt.Errorf("error serializing proof wrapper into JSON: %w", err)
	}
	g.log.Printlnf("Generation complete! Saving tree...")

	// Write the rewards tree to disk
	err = os.WriteFile(rewardsTreePath, wrapperBytes, 0644)
	if err != nil {
		return fmt.Errorf("error saving rewards tree file to %s: %w", rewardsTreePath, err)
	}

	g.log.Printlnf("Saved rewards snapshot file to %s", rewardsTreePath)
	g.log.Printlnf("Successfully generated rewards snapshot for interval %d.\n", index)

	return nil
}

// Prints handy stats about RP validators for a given epoch
func (g *treeGenerator) validatorStats(targetEpoch uint64) error {
	var rewardsEvent *rewards.RewardsEvent = nil

	targetBlock := targetEpoch * g.beaconConfig.SlotsPerEpoch

	if targetBlock > 0 {
		rewardsEvent = &rewards.RewardsEvent{ConsensusBlock: big.NewInt(0).SetUint64(targetBlock)}
	}

	state, err := g.getState(rewardsEvent)
	if err != nil {
		return err
	}

	validatorDetails := state.ValidatorDetails

	// Accumulators
	vStates := make(map[beacon.ValidatorState]*uint64)
	slashed := 0

	// Avgs
	tBalance := big.NewInt(0)
	nBalance := big.NewInt(0)

	for _, v := range validatorDetails {
		var acc *uint64

		nBalance = nBalance.Add(nBalance, big.NewInt(1))
		tBalance = tBalance.Add(tBalance, big.NewInt(0).SetUint64(v.Balance))

		if v.Slashed {
			slashed += 1
		}

		status := v.Status
		if status == "" {
			continue
		}

		if status == "exited_slashed" {
			g.log.Printlnf("index of slashee %d", v.Index)
		}

		acc, ok := vStates[status]
		if !ok {
			acc = new(uint64)
			vStates[status] = acc
		}
		*acc = *acc + 1
	}

	avgBalance := big.NewFloat(0).Quo(
		big.NewFloat(0).SetInt(tBalance),
		big.NewFloat(0).SetInt(nBalance),
	)
	avgBalance = avgBalance.Quo(avgBalance, big.NewFloat(1e9))
	avgBalanceF, _ := avgBalance.Float64()
	// Print summary
	g.log.Printlnf("Summary for %d validators at slot %d", nBalance.Uint64(), state.BeaconSlotNumber)
	g.log.Printlnf("avg_balance\t\t%f", avgBalanceF)
	g.log.Printlnf("total_slashed\t\t%d", slashed)
	for k, v := range vStates {
		g.log.Printlnf("%s\t\t%d", string(k), *v)
	}

	return nil
}

func (g *treeGenerator) overrideSnapshotDetails(details *snapshotDetails, state *state.NetworkState, endBlock uint64) error {
	// Get the genesis info
	genesisTime := state.BeaconConfig.GenesisTime
	genesisEpoch := state.BeaconConfig.GenesisEpoch
	slotsPerEpoch := state.BeaconConfig.SlotsPerEpoch
	secondsPerSlot := state.BeaconConfig.SecondsPerSlot

	endTime := (endBlock-(genesisEpoch*slotsPerEpoch))*secondsPerSlot + genesisTime
	targetEpoch := endBlock / slotsPerEpoch

	if endTime <= uint64(details.startTime.Unix()) {
		return fmt.Errorf("targetEpoch %d before current interval %d", targetEpoch, details.index)
	}

	beaconBlock, found, err := g.bn.GetBeaconBlock(fmt.Sprint(endBlock))
	if err != nil {
		return fmt.Errorf("error fetching beacon block %d: %w", endBlock, err)
	}
	if !found {
		panic("Should not be missing the endBlock after it was already found once")
	}
	if beaconBlock.ExecutionBlockNumber == 0 {
		return fmt.Errorf("target epoch block %d doesn't have an execution block. pre-merge?", endBlock)
	}

	details.endTime = time.Unix(int64(endTime), 0)
	details.snapshotBeaconBlock = endBlock
	details.snapshotElBlockHeader, err = g.rp.Client.HeaderByNumber(context.Background(), big.NewInt(0).SetUint64(beaconBlock.ExecutionBlockNumber))
	if err != nil {
		return fmt.Errorf("error fetching targetEpoch %d's EL block header for el block %d: %w", targetEpoch, beaconBlock.ExecutionBlockNumber, err)
	}

	return nil
}

// Generates a preview / dry run of the tree for the current interval, using the latest finalized state as the endpoint, or targetEpoch if provided.
func (g *treeGenerator) generatePartialTree(targetBlock uint64) error {
	var rewardsEvent *rewards.RewardsEvent = nil
	var err error

	if targetBlock > 0 {
		// Get beacon config
		if err != nil {
			return fmt.Errorf("error getting Beacon Config: %w", err)
		}
		rewardsEvent = &rewards.RewardsEvent{ConsensusBlock: big.NewInt(0).SetUint64(targetBlock)}

		g.log.Printlnf("Overriding the targeted slot to %d", targetBlock)
	}

	state, err := g.getState(rewardsEvent)
	if err != nil {
		return err
	}

	details, err := g.getSnapshotDetails(nil)
	if err != nil {
		return fmt.Errorf("error getting snapshot details: %w", err)
	}

	if targetBlock > 0 {
		targetEpoch := targetBlock / g.beaconConfig.SlotsPerEpoch
		// Override snapshot details with targetEpoch
		if err := g.overrideSnapshotDetails(&details, state, targetBlock); err != nil {
			return fmt.Errorf("error overriding target epoch %d: %w", targetEpoch, err)
		}

	}

	g.log.Printlnf("Generating a dry-run tree for the current interval (%d)", details.index)

	elBlockIndex := details.snapshotElBlockHeader.Number.Uint64()

	// Log
	g.log.Printlnf("Snapshot Beacon block = %d, EL block = %d, running from %s to %s\n", details.snapshotBeaconBlock, elBlockIndex, details.startTime, details.endTime)

	// Generate the rewards file
	treegen, err := rprewards.NewTreeGenerator(*g.log, "", g.rp, g.cfg, g.bn, details.index, details.startTime, details.endTime, details.snapshotBeaconBlock, details.snapshotElBlockHeader, details.intervalsPassed, state)
	if err != nil {
		return fmt.Errorf("error creating tree generator: %w", err)
	}
	rewardsFile, err := g.generateRewardsFile(treegen)
	if err != nil {
		return fmt.Errorf("error generating Merkle tree: %w", err)
	}
	for address, network := range rewardsFile.InvalidNetworkNodes {
		g.log.Printlnf("WARNING: Node %s has invalid network %d assigned! Using 0 (mainnet) instead.\n", address.Hex(), network)
	}

	err = g.writeFiles(rewardsFile, details.index)
	if err != nil {
		return err
	}

	return nil
}

// Approximates the rETH stakers' share of the Smoothing Pool's current balance
func (g *treeGenerator) approximateCurrentRethSpRewards() error {

	state, err := g.getState(nil)
	if err != nil {
		return err
	}

	details, err := g.getSnapshotDetails(nil)
	if err != nil {
		return fmt.Errorf("error getting snapshot details: %w", err)
	}

	elBlockIndex := details.snapshotElBlockHeader.Number.Uint64()

	// Log
	g.log.Printlnf("Snapshot Beacon block = %d, EL block = %d, running from %s to %s\n", details.snapshotBeaconBlock, elBlockIndex, details.startTime, details.endTime)

	// Get the Smoothing Pool contract's balance
	smoothingPoolContract, err := g.rp.GetContract("rocketSmoothingPool", nil)
	if err != nil {
		return fmt.Errorf("error getting smoothing pool contract: %w", err)
	}
	smoothingPoolBalance, err := g.rp.Client.BalanceAt(context.Background(), *smoothingPoolContract.Address, nil)
	if err != nil {
		return fmt.Errorf("error getting smoothing pool balance: %w", err)
	}

	// Approximate the balance
	treegen, err := rprewards.NewTreeGenerator(*g.log, "", g.rp, g.cfg, g.bn, details.index, details.startTime, details.endTime, details.snapshotBeaconBlock, details.snapshotElBlockHeader, details.intervalsPassed, state)
	if err != nil {
		return fmt.Errorf("error creating tree generator: %w", err)
	}

	var rETHShare *big.Int
	if g.ruleset == 0 {
		rETHShare, err = treegen.ApproximateStakerShareOfSmoothingPool()
	} else {
		rETHShare, err = treegen.ApproximateStakerShareOfSmoothingPoolWithRuleset(g.ruleset)
	}
	if err != nil {
		return fmt.Errorf("error approximating rETH stakers' share of the Smoothing Pool: %w", err)
	}
	g.log.Printlnf("Total ETH in the Smoothing Pool: %s wei (%.6f ETH)", smoothingPoolBalance.String(), eth.WeiToEth(smoothingPoolBalance))
	g.log.Printlnf("rETH stakers's share:            %s wei (%.6f ETH)", rETHShare.String(), eth.WeiToEth(rETHShare))

	return nil
}

func (g *treeGenerator) overrideRewardsEvent(rewardsEvent *rewards.RewardsEvent, targetBlock uint64) error {
	// Get the genesis info
	genesisTime := g.beaconConfig.GenesisTime
	genesisEpoch := g.beaconConfig.GenesisEpoch
	slotsPerEpoch := g.beaconConfig.SlotsPerEpoch
	secondsPerSlot := g.beaconConfig.SecondsPerSlot
	secondsPerEpoch := g.beaconConfig.SecondsPerEpoch

	endEpoch := rewardsEvent.ConsensusBlock.Uint64() / slotsPerEpoch
	targetEpoch := targetBlock / slotsPerEpoch

	// Clear the rewardsEvent, keeping a few fields we like
	*rewardsEvent = rewards.RewardsEvent{
		Index:             rewardsEvent.Index,
		IntervalsPassed:   rewardsEvent.IntervalsPassed,
		IntervalStartTime: rewardsEvent.IntervalStartTime,
	}

	// targetEpoch must fall between start epoch and end epoch
	startEpoch := ((uint64(rewardsEvent.IntervalStartTime.Unix()) - genesisTime) / secondsPerEpoch) + genesisEpoch
	if targetEpoch < startEpoch || targetEpoch > endEpoch {
		return fmt.Errorf("target epoch %d not in interval range %d - %d", targetEpoch, startEpoch, endEpoch)
	}

	if targetEpoch == endEpoch {
		// Just generate the full interval, since the requested endEpoch is the interval end
		return nil
	}

	// Rewrite rewardsEvent with the new end time
	rewardsEvent.ConsensusBlock = big.NewInt(0).SetUint64(targetBlock)
	beaconBlock, found, err := g.bn.GetBeaconBlock(fmt.Sprint(targetBlock))
	if err != nil {
		return fmt.Errorf("error querying bn for block %d: %w", targetBlock, err)
	}
	if !found {
		panic("Should not be missing the targetBlock after it was already found once")
	}
	if beaconBlock.ExecutionBlockNumber == 0 {
		return fmt.Errorf("target epoch block %d doesn't have an execution block. pre-merge?", targetBlock)
	}
	rewardsEvent.ExecutionBlock = big.NewInt(0).SetUint64(beaconBlock.ExecutionBlockNumber)
	endTime := (targetBlock-(genesisEpoch*slotsPerEpoch))*secondsPerSlot + genesisTime
	rewardsEvent.IntervalEndTime = time.Unix(int64(endTime), 0)

	return nil
}

// Recreates an existing tree for a past interval
func (g *treeGenerator) generatePastTree(index uint64, targetBlock uint64) error {
	targetEpoch := targetBlock / g.beaconConfig.SlotsPerEpoch

	// Find the event for this interval
	rewardsEvent, err := rprewards.GetRewardSnapshotEvent(g.rp, g.cfg, index)
	if err != nil {
		return fmt.Errorf("error getting rewards submission event for interval %d: %w", index, err)
	}
	g.log.Printlnf("Found rewards submission event: Beacon block %s, execution block %s", rewardsEvent.ConsensusBlock.String(), rewardsEvent.ExecutionBlock.String())

	// Apply overrides from targetEpoch, if set
	if targetEpoch > 0 {
		g.log.Printlnf("Overriding the target epoch to %d", targetEpoch)
		if err := g.overrideRewardsEvent(&rewardsEvent, targetBlock); err != nil {
			return fmt.Errorf("error override past interval %d with target epoch %d: %w", index, targetEpoch, err)
		}
	}

	state, err := g.getState(&rewardsEvent)
	if err != nil {
		return err
	}

	// Get the EL block
	elBlockHeader, err := g.rp.Client.HeaderByNumber(context.Background(), rewardsEvent.ExecutionBlock)
	if err != nil {
		return fmt.Errorf("error getting execution block: %w", err)
	}

	// Generate the rewards file
	start := time.Now()
	treegen, err := rprewards.NewTreeGenerator(*g.log, "", g.rp, g.cfg, g.bn, index, rewardsEvent.IntervalStartTime, rewardsEvent.IntervalEndTime, rewardsEvent.ConsensusBlock.Uint64(), elBlockHeader, rewardsEvent.IntervalsPassed.Uint64(), state)
	if err != nil {
		return fmt.Errorf("error creating tree generator: %w", err)
	}
	rewardsFile, err := g.generateRewardsFile(treegen)
	if err != nil {
		return fmt.Errorf("error generating Merkle tree: %w", err)
	}
	for address, network := range rewardsFile.InvalidNetworkNodes {
		g.log.Printlnf("WARNING: Node %s has invalid network %d assigned! Using 0 (mainnet) instead.", address.Hex(), network)
	}
	g.log.Printlnf("Finished in %s", time.Since(start).String())

	// Validate the Merkle root
	if targetEpoch == 0 {
		root := common.BytesToHash(rewardsFile.MerkleTree.Root())
		if root != rewardsEvent.MerkleRoot {
			g.log.Printlnf("WARNING: your Merkle tree had a root of %s, but the canonical Merkle tree's root was %s. This file will not be usable for claiming rewards.", root.Hex(), rewardsEvent.MerkleRoot.Hex())
		} else {
			g.log.Printlnf("Your Merkle tree's root of %s matches the canonical root! You will be able to use this file for claiming rewards.", rewardsFile.MerkleRoot)
		}
	}

	err = g.writeFiles(rewardsFile, index)
	if err != nil {
		return err
	}

	return nil

}

// Get the finalized slot for the given target epoch, or the latest one if there isn't a target epoch
func getFinalizedSlot(log log.ColorLogger, bn beacon.Client, targetEpoch *uint64) (uint64, uint64, time.Time, error) {

	// Get the config
	eth2Config, err := bn.GetEth2Config()
	if err != nil {
		return 0, 0, time.Time{}, fmt.Errorf("error getting Beacon config: %w", err)
	}
	genesisTime := time.Unix(int64(eth2Config.GenesisTime), 0)

	// Get the target epoch details

	// Get the beacon head
	beaconHead, err := bn.GetBeaconHead()
	if err != nil {
		return 0, 0, time.Time{}, fmt.Errorf("error getting Beacon head: %w", err)
	}

	// Get the latest finalized slot that existed
	finalizedEpoch := beaconHead.FinalizedEpoch

	// Sanity check the target epoch
	if targetEpoch != nil {
		if *targetEpoch > finalizedEpoch {
			return 0, 0, time.Time{}, fmt.Errorf("target epoch %d is not finalized yet; latest finalized epoch is %d", *targetEpoch, finalizedEpoch)
		}
	}

	// Get the target slot
	var finalizedSlot uint64
	if targetEpoch == nil {
		finalizedSlot = (finalizedEpoch+1)*eth2Config.SlotsPerEpoch - 1
	} else {
		finalizedSlot = (*targetEpoch+1)*eth2Config.SlotsPerEpoch - 1
	}

	for {
		// Try to get the current block
		block, exists, err := bn.GetBeaconBlock(fmt.Sprint(finalizedSlot))
		if err != nil {
			return 0, 0, time.Time{}, fmt.Errorf("error getting Beacon block %d: %w", finalizedSlot, err)
		}

		// If the block was missing, try the previous one
		if !exists {
			log.Printlnf("Slot %d was missing, trying the previous one...", finalizedSlot)
			finalizedSlot--
			continue
		}

		blockTime := genesisTime.Add(time.Duration(finalizedSlot*eth2Config.SecondsPerSlot) * time.Second)
		return finalizedSlot, block.ExecutionBlockNumber, blockTime, nil
	}

}

// Get the details of the rewards snapshot at the given block
func (g *treeGenerator) getSnapshotDetails(opts *bind.CallOpts) (snapshotDetails, error) {
	// Get the interval index
	indexBig, err := rewards.GetRewardIndex(g.rp, opts)
	if err != nil {
		return snapshotDetails{}, fmt.Errorf("error getting current reward index: %w", err)
	}
	index := indexBig.Uint64()

	// Get the start time for the current interval, and how long an interval is supposed to take
	startTime, err := rewards.GetClaimIntervalTimeStart(g.rp, opts)
	if err != nil {
		return snapshotDetails{}, fmt.Errorf("error getting claim interval start time: %w", err)
	}
	intervalTime, err := rewards.GetClaimIntervalTime(g.rp, opts)
	if err != nil {
		return snapshotDetails{}, fmt.Errorf("error getting claim interval time: %w", err)
	}

	// Get the block header for the target block
	var targetBlockNumber *big.Int
	if opts == nil {
		targetBlockNumber = nil
	} else {
		targetBlockNumber = opts.BlockNumber
	}
	blockHeader, err := g.rp.Client.HeaderByNumber(context.Background(), targetBlockNumber)
	if err != nil {
		return snapshotDetails{}, fmt.Errorf("error getting latest block header: %w", err)
	}

	// Calculate the intervals passed
	blockTime := time.Unix(int64(blockHeader.Time), 0)
	timeSinceStart := blockTime.Sub(startTime)
	intervalsPassed := timeSinceStart / intervalTime
	endTime := time.Now()

	// Get the latest finalized Beacon block
	snapshotBeaconBlock, elBlockNumber, beaconBlockTime, err := getFinalizedSlot(*g.log, g.bn, nil)
	if err != nil {
		return snapshotDetails{}, fmt.Errorf("error getting latest finalized slot: %w", err)
	}

	// Get the number of the EL block matching the CL snapshot block
	var snapshotElBlockHeader *types.Header
	if elBlockNumber == 0 {
		// No EL data so the Merge hasn't happened yet, figure out the EL block based on the Epoch ending time
		snapshotElBlockHeader, err = rprewards.GetELBlockHeaderForTime(beaconBlockTime, g.rp)
		if err != nil {
			return snapshotDetails{}, fmt.Errorf("error getting EL block for time %s: %w", beaconBlockTime, err)
		}
	} else {
		snapshotElBlockHeader, err = g.rp.Client.HeaderByNumber(context.Background(), big.NewInt(int64(elBlockNumber)))
		if err != nil {
			return snapshotDetails{}, fmt.Errorf("error getting EL block %d: %w", elBlockNumber, err)
		}
	}

	return snapshotDetails{
		index:                 index,
		startTime:             startTime,
		endTime:               endTime,
		snapshotBeaconBlock:   snapshotBeaconBlock,
		snapshotElBlockHeader: snapshotElBlockHeader,
		intervalsPassed:       uint64(intervalsPassed),
	}, nil
}

func (g *treeGenerator) printNetworkInfo(opts *bind.CallOpts) error {
	details, err := g.getSnapshotDetails(opts)
	if err != nil {
		return fmt.Errorf("error getting network details for snapshot: %w", err)
	}

	// Get a snapshot of the network state at that interval
	mgr, err := state.NewNetworkStateManager(g.rp, g.cfg, g.rp.Client, g.bn, g.log)
	if err != nil {
		return fmt.Errorf("error creating network state manager: %w", err)
	}
	state, err := mgr.GetStateForSlot(details.snapshotBeaconBlock)
	if err != nil {
		return fmt.Errorf("error getting network state: %w", err)
	}

	generator, err := rprewards.NewTreeGenerator(*g.log, "", g.rp, g.cfg, g.bn, details.index, details.startTime, details.endTime, details.snapshotBeaconBlock, details.snapshotElBlockHeader, details.intervalsPassed, state)
	if err != nil {
		return fmt.Errorf("error creating generator: %w", err)
	}

	g.log.Println()
	g.log.Println("=== Network Details ===")
	g.log.Printlnf("Current index:        %d", details.index)
	g.log.Printlnf("Start Time:           %s", details.startTime)

	// Find the event for the previous interval
	if details.index > 0 {
		rewardsEvent, err := rprewards.GetRewardSnapshotEvent(g.rp, g.cfg, details.index-1)
		if err != nil {
			return fmt.Errorf("error getting rewards submission event for previous interval (%d): %w", details.index-1, err)
		}
		g.log.Printlnf("Start Beacon Slot:    %d", rewardsEvent.ConsensusBlock.Uint64()+1)
		g.log.Printlnf("Start EL Block:       %d", rewardsEvent.ExecutionBlock.Uint64()+1)
	}

	g.log.Printlnf("End Time:             %s", details.endTime)
	g.log.Printlnf("Snapshot Beacon Slot: %d", details.snapshotBeaconBlock)
	g.log.Printlnf("Snapshot EL Block:    %s", details.snapshotElBlockHeader.Number.String())
	g.log.Printlnf("Intervals Passed:     %d", details.intervalsPassed)
	g.log.Printlnf("Tree Ruleset:         v%d", generator.GetGeneratorRulesetVersion())
	g.log.Printlnf("Approximator Ruleset: v%d", generator.GetApproximatorRulesetVersion())

	return nil
}

// Configure HTTP transport settings
func configureHTTP() {

	// The watchtower daemon makes a large number of concurrent RPC requests to the Eth1 client
	// The HTTP transport is set to cache connections for future re-use equal to the maximum expected number of concurrent requests
	// This prevents issues related to memory consumption and address allowance from repeatedly opening and closing connections
	http.DefaultTransport.(*http.Transport).MaxIdleConnsPerHost = MaxConcurrentEth1Requests

}
