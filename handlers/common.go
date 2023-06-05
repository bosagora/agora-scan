package handlers

import (
	"encoding/hex"
	"encoding/json"
	"eth2-exporter/db"
	"eth2-exporter/price"
	"eth2-exporter/services"
	"eth2-exporter/types"
	"eth2-exporter/utils"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/gorilla/sessions"
	"github.com/lib/pq"
)

var pkeyRegex = regexp.MustCompile("[^0-9A-Fa-f]+")

func GetValidatorOnlineThresholdSlot() uint64 {
	latestProposedSlot := services.LatestProposedSlot()
	threshold := utils.Config.Chain.Config.SlotsPerEpoch * 2

	var validatorOnlineThresholdSlot uint64
	if latestProposedSlot < 1 || latestProposedSlot < threshold {
		validatorOnlineThresholdSlot = 0
	} else {
		validatorOnlineThresholdSlot = latestProposedSlot - threshold
	}

	return validatorOnlineThresholdSlot
}

// GetValidatorEarnings will return the earnings (last day, week, month and total) of selected validators
func GetValidatorEarnings(validators []uint64, currency string) (*types.ValidatorEarnings, error) {
	validatorsPQArray := pq.Array(validators)
	latestEpoch := int64(services.LatestEpoch())
	lastDayEpoch := latestEpoch - 225
	lastWeekEpoch := latestEpoch - 225*7
	lastMonthEpoch := latestEpoch - 225*31

	if lastDayEpoch < 0 {
		lastDayEpoch = 0
	}
	if lastWeekEpoch < 0 {
		lastWeekEpoch = 0
	}
	if lastMonthEpoch < 0 {
		lastMonthEpoch = 0
	}

	balances := []*types.Validator{}

	err := db.ReaderDb.Select(&balances, `SELECT 
			   COALESCE(balance, 0) AS balance, 
			   COALESCE(balanceactivation, 0) AS balanceactivation, 
			   COALESCE(balance1d, 0) AS balance1d, 
			   COALESCE(balance7d, 0) AS balance7d, 
			   COALESCE(balance31d , 0) AS balance31d,
			   COALESCE(withdrawal, 0) AS withdrawal, 
			   COALESCE(withdrawal1d, 0) AS withdrawal1d, 
			   COALESCE(withdrawal7d, 0) AS withdrawal7d,
			   COALESCE(withdrawal31d, 0) AS withdrawal31d,
       			activationepoch,
       			pubkey
		FROM validators WHERE validatorindex = ANY($1)`, validatorsPQArray)
	if err != nil {
		logger.Error(err)
		return nil, err
	}

	deposits := []struct {
		Epoch     int64
		Amount    int64
		Publickey []byte
	}{}

	err = db.ReaderDb.Select(&deposits, "SELECT block_slot / 32 AS epoch, amount, publickey FROM blocks_deposits WHERE publickey IN (SELECT pubkey FROM validators WHERE validatorindex = ANY($1))", validatorsPQArray)
	if err != nil {
		return nil, err
	}

	depositsMap := make(map[string]map[int64]int64)
	for _, d := range deposits {
		if _, exists := depositsMap[fmt.Sprintf("%x", d.Publickey)]; !exists {
			depositsMap[fmt.Sprintf("%x", d.Publickey)] = make(map[int64]int64)
		}
		depositsMap[fmt.Sprintf("%x", d.Publickey)][d.Epoch] += d.Amount
	}

	var earningsTotal int64
	var earningsLastDay int64
	var earningsLastWeek int64
	var earningsLastMonth int64
	var apr float64
	var totalDeposits int64

	for _, balance := range balances {
		if int64(balance.ActivationEpoch) >= latestEpoch {
			continue
		}
		for epoch, deposit := range depositsMap[fmt.Sprintf("%x", balance.PublicKey)] {
			totalDeposits += deposit

			if epoch > int64(balance.ActivationEpoch) {
				earningsTotal -= deposit
			}
			if epoch > lastDayEpoch && epoch > int64(balance.ActivationEpoch) {
				earningsLastDay -= deposit
			}
			if epoch > lastWeekEpoch && epoch > int64(balance.ActivationEpoch) {
				earningsLastWeek -= deposit
			}
			if epoch > lastMonthEpoch && epoch > int64(balance.ActivationEpoch) {
				earningsLastMonth -= deposit
			}
		}

		if int64(balance.ActivationEpoch) > lastDayEpoch {
			balance.Balance1d = balance.BalanceActivation
		}
		if int64(balance.ActivationEpoch) > lastWeekEpoch {
			balance.Balance7d = balance.BalanceActivation
		}
		if int64(balance.ActivationEpoch) > lastMonthEpoch {
			balance.Balance31d = balance.BalanceActivation
		}
		balance.Balance += balance.Withdrawal
		balance.Balance1d += balance.Withdrawal1d
		balance.Balance7d += balance.Withdrawal7d
		balance.Balance31d += balance.Withdrawal31d

		earningsTotal += int64(balance.Balance) - int64(balance.BalanceActivation)
		earningsLastDay += int64(balance.Balance) - int64(balance.Balance1d)
		earningsLastWeek += int64(balance.Balance) - int64(balance.Balance7d)
		earningsLastMonth += int64(balance.Balance) - int64(balance.Balance31d)
	}

	apr = (((float64(earningsLastWeek) / 1e9) / (float64(totalDeposits) / 1e9)) * 365) / 7
	if apr < float64(-1) {
		apr = float64(-1)
	}

	return &types.ValidatorEarnings{
		Total:                earningsTotal,
		LastDay:              earningsLastDay,
		LastWeek:             earningsLastWeek,
		LastMonth:            earningsLastMonth,
		APR:                  apr,
		TotalDeposits:        totalDeposits,
		LastDayFormatted:     utils.FormatIncome(earningsLastDay, currency),
		LastWeekFormatted:    utils.FormatIncome(earningsLastWeek, currency),
		LastMonthFormatted:   utils.FormatIncome(earningsLastMonth, currency),
		TotalFormatted:       utils.FormatIncome(earningsTotal, currency),
		TotalChangeFormatted: utils.FormatIncome(earningsTotal+totalDeposits, currency),
	}, nil
}

// LatestState will return common information that about the current state of the eth2 chain
func LatestState(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	currency := GetCurrency(r)
	data := services.LatestState()
	// data.Currency = currency
	data.EthPrice = price.GetEthPrice(currency)
	data.EthRoundPrice = price.GetEthRoundPrice(data.EthPrice)
	data.EthTruncPrice = utils.KFormatterEthPrice(data.EthRoundPrice)

	err := json.NewEncoder(w).Encode(data)

	if err != nil {
		logger.Errorf("error sending latest index page data: %v", err)
		http.Error(w, "Internal server error", http.StatusServiceUnavailable)
		return
	}
}

func GetCurrency(r *http.Request) string {
	if cookie, err := r.Cookie("currency"); err == nil {
		return cookie.Value
	}

	return "BOA"
}

func GetCurrencySymbol(r *http.Request) string {

	cookie, err := r.Cookie("currency")
	if err != nil {
		return "$"
	}

	switch cookie.Value {
	case "AUD":
		return "A$"
	case "CAD":
		return "C$"
	case "CNY":
		return "¥"
	case "EUR":
		return "€"
	case "GBP":
		return "£"
	case "JPY":
		return "¥"
	case "RUB":
		return "₽"
	default:
		return "$"
	}
}

func GetCurrentPrice(r *http.Request) uint64 {
	cookie, err := r.Cookie("currency")
	if err != nil {
		return price.GetEthRoundPrice(price.GetEthPrice("USD"))
	}

	if cookie.Value == "BOA" {
		return price.GetEthRoundPrice(price.GetEthPrice("USD"))
	}
	return price.GetEthRoundPrice(price.GetEthPrice(cookie.Value))
}

func GetCurrentExactPrice(r *http.Request) float64 {
	cookie, err := r.Cookie("currency")
	if err != nil {
		return price.GetEthPrice("USD")
	}

	if cookie.Value == "BOA" {
		return price.GetEthPrice("USD")
	}
	return price.GetEthPrice(cookie.Value)
}

func GetCurrentPriceFormatted(r *http.Request) string {
	userAgent := r.Header.Get("User-Agent")
	userAgent = strings.ToLower(userAgent)
	price := GetCurrentPrice(r)
	if strings.Contains(userAgent, "android") || strings.Contains(userAgent, "iphone") || strings.Contains(userAgent, "windows phone") {
		return fmt.Sprintf("%s", utils.KFormatterEthPrice(price))
	}
	return fmt.Sprintf("%s", utils.FormatAddCommas(uint64(price)))
}

func GetCurrentExactPriceFormatted(r *http.Request) string {
	userAgent := r.Header.Get("User-Agent")
	userAgent = strings.ToLower(userAgent)
	price := GetCurrentExactPrice(r)
	// if strings.Contains(userAgent, "android") || strings.Contains(userAgent, "iphone") || strings.Contains(userAgent, "windows phone") {
	// 	return fmt.Sprintf("%s", utils.KFormatterEthPrice(price))
	// }
	retval := fmt.Sprintf("%s", utils.FormatFloat(price, 2))
	logger.Infof("BOA price is %s", retval)
	return retval
}

func GetTruncCurrentPriceFormatted(r *http.Request) string {
	price := GetCurrentPrice(r)
	symbol := GetCurrencySymbol(r)
	return fmt.Sprintf("%s %s", symbol, utils.KFormatterEthPrice(price))
}

// GetValidatorIndexFrom gets the validator index from users input
func GetValidatorIndexFrom(userInput string) (pubKey []byte, validatorIndex uint64, err error) {
	validatorIndex, err = strconv.ParseUint(userInput, 10, 64)
	if err == nil {
		pubKey, err = db.GetValidatorPublicKey(validatorIndex)
		return
	}

	pubKey, err = hex.DecodeString(strings.Replace(userInput, "0x", "", -1))
	if err == nil {
		validatorIndex, err = db.GetValidatorIndex(pubKey)
		return
	}
	return
}

func DataTableStateChanges(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	user, session, err := getUserSession(r)
	if err != nil {
		logger.Errorf("error retrieving session: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	response := &types.ApiResponse{}
	response.Status = "ERROR"

	defer json.NewEncoder(w).Encode(response)

	settings := types.DataTableSaveState{}
	err = json.NewDecoder(r.Body).Decode(&settings)
	if err != nil {
		logger.Errorf("error saving data table state could not parse body: %v", err)
		response.Status = "error saving table state"
		return
	}

	key := settings.Key
	if len(key) == 0 {
		logger.Errorf("no key provided")
		response.Status = "error saving table state"
		return
	}

	if !user.Authenticated {
		dataTableStatePrefix := "table:state:" + utils.GetNetwork() + ":"
		key = dataTableStatePrefix + key
		count := 0
		for k, _ := range session.Values {
			k, ok := k.(string)
			if ok && strings.HasPrefix(k, dataTableStatePrefix) {
				count += 1
			}
		}
		if count > 50 {
			_, ok := session.Values[key]
			if !ok {
				logger.Errorf("error maximum number of datatable states stored in session")
				return
			}
		}
		session.Values[key] = settings

		err := session.Save(r, w)
		if err != nil {
			logger.WithError(err).Errorf("error updating session with key: %v and value: %v", key, settings)
		}

	} else {
		err = db.SaveDataTableState(user.UserID, settings.Key, settings)
		if err != nil {
			logger.Errorf("error saving data table state could save values to db: %v", err)
			response.Status = "error saving table state"
			return
		}
	}

	response.Status = "OK"
	response.Data = ""
}

func GetDataTableState(user *types.User, session *sessions.Session, tableKey string) (*types.DataTableSaveState, error) {
	if user.Authenticated {
		state, err := db.GetDataTablesState(user.UserID, tableKey)
		if err != nil {
			return nil, err
		}
		return state, nil
	}
	stateRaw, exists := session.Values["table:state:"+utils.GetNetwork()+":"+tableKey]
	if !exists {
		return nil, nil
	}
	state, ok := stateRaw.(types.DataTableSaveState)
	if !ok {
		return nil, fmt.Errorf("error parsing session value into type DataTableSaveState")
	}
	return &state, nil
}

func GetWithdrawableCountFromCursor(epoch uint64, validatorindex uint64, cursor uint64) (uint64, error) {
	// the validators' balance will not be checked here as this is only a rough estimation
	// checking the balance for hundreds of thousands of validators is too expensive

	var maxValidatorIndex uint64
	err := db.WriterDb.Get(&maxValidatorIndex, "SELECT COALESCE(MAX(validatorindex), 0) FROM validators")
	if err != nil {
		return 0, fmt.Errorf("error getting withdrawable validator count from cursor: %w", err)
	}

	if maxValidatorIndex == 0 {
		return 0, nil
	}

	activeValidators := services.LatestIndexPageData().ActiveValidators
	if activeValidators == 0 {
		activeValidators = maxValidatorIndex
	}

	if validatorindex > cursor {
		// if the validatorindex is after the cursor, simply return the number of validators between the cursor and the validatorindex
		// the returned data is then scaled using the number of currently active validators in order to account for exited / entering validators
		return (validatorindex - cursor) * activeValidators / maxValidatorIndex, nil
	} else if validatorindex < cursor {
		// if the validatorindex is before the cursor (wraparound case) return the number of validators between the cursor and the most recent validator plus the amount of validators from the validator 0 to the validatorindex
		// the returned data is then scaled using the number of currently active validators in order to account for exited / entering validators
		return (maxValidatorIndex - cursor + validatorindex) * activeValidators / maxValidatorIndex, nil
	} else {
		return 0, nil
	}
}
