// Package exchange manages the binded deposit address and skycoin address,
// when get new deposits from scanner, exchange will find the corresponding
// skycoin address, and use skycoin sender to send skycoins in given rate.
package exchange

import (
	"fmt"

	"github.com/boltdb/bolt"
	"github.com/skycoin/teller/src/logger"
	"github.com/skycoin/teller/src/service/scanner"
	"github.com/skycoin/teller/src/service/sender"
)

var (
	coinValueBktName      = []byte("coinValue")
	exchangeLogBktName    = []byte("exchangeLog")
	unconfirmedTxsBktName = []byte("unconfirmed_txs")
)

// SkySender provids apis for sending skycoin
type SkySender interface {
	Send(destAddr string, coins int64, opt *sender.SendOption) (string, error)
}

// BtcScanner provids apis for interact with scan service
type BtcScanner interface {
	AddDepositAddress(addr string) error
	GetDepositValue() <-chan scanner.DepositValue
}

func calculateSkyValue(btcValue float64, rate float64) int64 {
	return int64(btcValue * rate)
}

// Service manages coin exchange between deposits and skycoin
type Service struct {
	logger.Logger
	cfg     Config
	scanner BtcScanner // scanner provides apis for interacting with scan service
	sender  SkySender  // sender provides apis for sending skycoin
	store   *store     // deposit info storage
	quit    chan struct{}
}

// Config exchange config struct
type Config struct {
	DB   *bolt.DB
	Log  logger.Logger
	Rate int64 // sky_btc rate
}

// NewService creates exchange service
func NewService(cfg Config, scanner BtcScanner, sender SkySender) *Service {
	s, err := newStore(cfg.DB)
	if err != nil {
		panic(err)
	}

	return &Service{
		Logger:  cfg.Log,
		cfg:     cfg,
		scanner: scanner,
		sender:  sender,
		store:   s,
		quit:    make(chan struct{}),
	}
}

// Run starts the exchange process
func (s *Service) Run() error {
	for {
		select {
		case <-s.quit:
			return nil
		case dv, ok := <-s.scanner.GetDepositValue():
			if !ok {
				s.Println("Scan service closed")
				return nil
			}

			// get deposit info of given btc address
			dpi, ok := s.store.GetDepositInfo(dv.Address)
			if !ok {
				s.Printf("Deposit info of btc address %s doesn't exist\n", dv.Address)
				continue
			}

			// only update the status that are under waiting_sky_send
			if dpi.Status >= statusWaitSkySend {
				// TODO: this might mean the user deposit btcoin the btc address multiple times
				s.Printf("Deposit status of btc address %s already >= %s\n", dv.Address, statusString[statusWaitSkySend])
				continue
			}

			// update status to waiting_sky_send
			err := s.store.UpdateDepositInfo(dv.Address, func(dpi depositInfo) depositInfo {
				dpi.Status = statusWaitSkySend
				return dpi
			})

			if err != nil {
				s.Printf("Update deposit status of btc address %s failed: %v\n", dv.Address, err)
				continue
			}

			// send skycoins
			// get binded skycoin address
			skyAddr, ok := s.store.GetBindAddress(dv.Address)
			if !ok {
				s.Println("Find no bind skycoin address for btc address", dv.Address)
			}

			// try to send skycoin
			skyAmt := calculateSkyValue(dv.Value, float64(s.cfg.Rate))

			txid, err := s.sender.Send(skyAddr, skyAmt, nil)
			if err != nil && err != sender.ErrServiceClosed {
				return fmt.Errorf("Send %d skycoin to %s failed: %v", skyAmt, skyAddr, err)
			}

			s.Printf("Send %d skycoin to %s success, txid=%s, deposit address=%s\n",
				skyAmt, skyAddr, txid, dv.Address)

			// update the txid
			if er := s.store.UpdateDepositInfo(dv.Address, func(dpi depositInfo) depositInfo {
				dpi.Status = statusDone
				dpi.Txid = txid
				return dpi
			}); er != nil {
				s.Printf("Update deposit info for btc address %s failed: %v\n", dv.Address, er)
			}

			if err == sender.ErrServiceClosed {
				return nil
			}
		}
	}
}

// Shutdown close the exchange service
func (s *Service) Shutdown() {
	close(s.quit)
}

func (s *Service) addDepositInfo(btcAddr, skyAddr string) error {
	_, err := s.store.AddDepositInfo(depositInfo{
		BtcAddress: btcAddr,
		SkyAddress: skyAddr,
	})
	return err
}