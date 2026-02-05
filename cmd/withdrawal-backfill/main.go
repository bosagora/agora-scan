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
	lookback := flag.Uint("epochs", 20, "Number of epochs to backfill per cycle (sequential from progress)")
	startFromEpoch := flag.Uint64("start-from-epoch", 0, "If progress is -1, set it to (start-from-epoch-1) so backfill begins at this epoch (e.g. 249012); 0 = start from epoch 0")
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

	if err := db.EnsureWithdrawalBackfillProgressTable(); err != nil {
		logrus.Fatalf("ensure withdrawal_backfill_progress table: %v", err)
	}
	if *startFromEpoch > 0 {
		lastDone, err := db.GetWithdrawalBackfillProgress()
		if err != nil {
			logrus.Fatalf("get withdrawal backfill progress: %v", err)
		}
		if lastDone < 0 {
			initEpoch := int64(*startFromEpoch) - 1
			if initEpoch < 0 {
				initEpoch = 0
			}
			if err := db.SetWithdrawalBackfillProgress(uint64(initEpoch)); err != nil {
				logrus.Fatalf("set initial progress to %d: %v", initEpoch, err)
			}
			logrus.Infof("initial progress set to epoch %d (backfill will start from epoch %d)", initEpoch, *startFromEpoch)
		}
	}

	slotsPerEpoch := utils.Config.Chain.Config.SlotsPerEpoch
	if slotsPerEpoch == 0 {
		slotsPerEpoch = 32
	}

	logrus.Infof("withdrawal-backfill started: interval=%v, epochs per cycle=%d (sequential from low epoch, no duplicate)", *interval, *lookback)

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

	lastDone, err := db.GetWithdrawalBackfillProgress()
	if err != nil {
		logrus.Errorf("get withdrawal backfill progress: %v", err)
		return
	}
	if lastDone < 0 {
		// No progress yet: treat as "already at head", so we only backfill new epochs from now on.
		if err := db.SetWithdrawalBackfillProgress(maxEpoch); err != nil {
			logrus.Errorf("set initial progress to maxEpoch %d: %v", maxEpoch, err)
			return
		}
		logrus.Infof("initial progress set to current max epoch %d (will backfill from epoch %d onward)", maxEpoch, maxEpoch+1)
		lastDone = int64(maxEpoch)
	}
	startEpoch := uint64(lastDone + 1)

	if startEpoch > maxEpoch {
		return
	}
	endEpoch := startEpoch + lookback - 1
	if endEpoch > maxEpoch {
		endEpoch = maxEpoch
	}
	// Process low → high, sequential; record each so we never redo and never skip.
	var lastWithdrawals map[uint64]uint64
	for epoch := startEpoch; epoch <= endEpoch; epoch++ {
		endSlot := (epoch+1)*slotsPerEpoch - 1
		t0 := time.Now()
		withdrawals, err := db.GetAllValidatorTotalWithdrawals(endSlot)
		if err != nil {
			logrus.Errorf("GetAllValidatorTotalWithdrawals(epoch=%d, slot=%d): %v", epoch, endSlot, err)
			return
		}
		if err := db.UpdateValidatorBalancesWithdrawal(epoch, withdrawals); err != nil {
			logrus.Errorf("UpdateValidatorBalancesWithdrawal(epoch=%d): %v", epoch, err)
			return
		}
		if err := db.SetWithdrawalBackfillProgress(epoch); err != nil {
			logrus.Errorf("SetWithdrawalBackfillProgress(epoch=%d): %v", epoch, err)
			return
		}
		lastWithdrawals = withdrawals
		logrus.Infof("backfilled withdrawal epoch=%d slot=%d validators=%d took %v (progress recorded)", epoch, endSlot, len(withdrawals), time.Since(t0))
	}
	if lastWithdrawals != nil {
		if err := db.UpdateValidatorsWithdrawal(lastWithdrawals); err != nil {
			logrus.Errorf("UpdateValidatorsWithdrawal: %v", err)
		} else {
			logrus.Infof("updated validators.withdrawal for %d validators", len(lastWithdrawals))
		}
	}
}
