// withdrawal-backfill is a dedicated process that backfills validator withdrawal
// data into validator_balances_p and validator_balances_recent. Run the main
// indexer with skipValidatorWithdrawals: true so it stays fast; this process
// fills in withdrawal (and total_balance) in the background.
package main

import (
	"eth2-exporter/db"
	"eth2-exporter/types"
	"eth2-exporter/utils"
	"flag"
	"time"

	"github.com/sirupsen/logrus"
)

func main() {
	configPath := flag.String("config", "", "Path to the config file")
	interval := flag.Duration("interval", 15*time.Minute, "How often to run a backfill cycle")
	lookback := flag.Uint("epochs", 20, "Number of recent epochs to backfill per cycle")
	flag.Parse()

	cfg := &types.Config{}
	if err := utils.ReadConfig(cfg, *configPath); err != nil {
		logrus.Fatalf("reading config: %v", err)
	}
	utils.Config = cfg

	db.MustInitDB(
		&types.DatabaseConfig{
			Username: cfg.WriterDatabase.Username,
			Password: cfg.WriterDatabase.Password,
			Name:     cfg.WriterDatabase.Name,
			Host:     cfg.WriterDatabase.Host,
			Port:     cfg.WriterDatabase.Port,
		},
		&types.DatabaseConfig{
			Username: cfg.ReaderDatabase.Username,
			Password: cfg.ReaderDatabase.Password,
			Name:     cfg.ReaderDatabase.Name,
			Host:     cfg.ReaderDatabase.Host,
			Port:     cfg.ReaderDatabase.Port,
		},
	)
	defer db.ReaderDb.Close()
	defer db.WriterDb.Close()

	slotsPerEpoch := utils.Config.Chain.Config.SlotsPerEpoch
	if slotsPerEpoch == 0 {
		slotsPerEpoch = 32
	}

	logrus.Infof("withdrawal-backfill started: interval=%v, epochs per cycle=%d (each epoch ~4min for GetAllValidatorTotalWithdrawals)", *interval, *lookback)

	for {
		t0 := time.Now()
		runBackfill(slotsPerEpoch, uint64(*lookback))
		elapsed := time.Since(t0)
		logrus.Infof("backfill cycle finished in %v, sleeping %v", elapsed, *interval)
		time.Sleep(*interval)
	}
}

func runBackfill(slotsPerEpoch, lookback uint64) {
	var maxEpoch uint64
	if err := db.ReaderDb.Get(&maxEpoch, "SELECT COALESCE(MAX(epoch), 0) FROM validator_balances_p"); err != nil {
		logrus.Errorf("getting max epoch: %v", err)
		return
	}
	if maxEpoch == 0 {
		return
	}

	start := uint64(0)
	if maxEpoch >= lookback {
		start = maxEpoch - lookback + 1
	}
	var lastWithdrawals map[uint64]uint64
	for epoch := start; epoch <= maxEpoch; epoch++ {
		endSlot := (epoch+1)*slotsPerEpoch - 1
		t0 := time.Now()
		withdrawals, err := db.GetAllValidatorTotalWithdrawals(endSlot)
		if err != nil {
			logrus.Errorf("GetAllValidatorTotalWithdrawals(epoch=%d, slot=%d): %v", epoch, endSlot, err)
			continue
		}
		elapsed := time.Since(t0)
		if err := db.UpdateValidatorBalancesWithdrawal(epoch, withdrawals); err != nil {
			logrus.Errorf("UpdateValidatorBalancesWithdrawal(epoch=%d): %v", epoch, err)
			continue
		}
		lastWithdrawals = withdrawals
		logrus.Infof("backfilled withdrawal epoch=%d slot=%d validators=%d took %v", epoch, endSlot, len(withdrawals), elapsed)
	}
	// Keep validators.withdrawal (cumulative) in sync using the latest epoch's totals
	if lastWithdrawals != nil {
		if err := db.UpdateValidatorsWithdrawal(lastWithdrawals); err != nil {
			logrus.Errorf("UpdateValidatorsWithdrawal: %v", err)
		} else {
			logrus.Infof("updated validators.withdrawal for %d validators", len(lastWithdrawals))
		}
	}
}
