package states

import (
	"context"
	"encoding/base64"
	"errors"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/RiemaLabs/modular-indexer-committee/apis"
	"github.com/RiemaLabs/modular-indexer-committee/checkpoint"
	"github.com/RiemaLabs/modular-indexer-committee/ord/getter"
	"github.com/ethereum/go-verkle"

	"github.com/RiemaLabs/modular-indexer-light/internal/checkpoints"
	"github.com/RiemaLabs/modular-indexer-light/internal/clients/committee"
	"github.com/RiemaLabs/modular-indexer-light/internal/clients/ordi"
	"github.com/RiemaLabs/modular-indexer-light/internal/configs"
	"github.com/RiemaLabs/modular-indexer-light/internal/logs"
)

// TODO: Medium. Uniform the error report.

type Status int

const (
	StatusActive Status = iota + 1
	StatusSync
	StatusVerify
)

func (s Status) String() string {
	switch s {
	case StatusActive:
		return "ready"
	case StatusSync:
		return "syncing"
	case StatusVerify:
		return "verifying"
	default:
		return ""
	}
}

type State struct {
	State atomic.Int64

	denyListPath string

	providers []checkpoints.CheckpointProvider

	// The consistent check point at the current height - 1.
	lastCheckpoint *configs.CheckpointExport

	// The checkpoints got from providers at the current height.
	currentCheckpoints []*configs.CheckpointExport

	// The number of effective providers should exceed the minimum required.
	minimalCheckpoint int

	// timeout for request checkpoint.
	timeout time.Duration

	sync.RWMutex
}

var S *State

func New(
	denyListPath string,
	providers []checkpoints.CheckpointProvider,
	lastCheckpoint *configs.CheckpointExport,
	minimalCheckpoint int,
	fetchTimeout time.Duration,
) *State {
	return &State{
		denyListPath:       denyListPath,
		providers:          providers,
		lastCheckpoint:     lastCheckpoint,
		currentCheckpoints: make([]*configs.CheckpointExport, len(providers)),
		minimalCheckpoint:  minimalCheckpoint,
		timeout:            fetchTimeout,
	}
}

func Init(
	denyListPath string,
	providers []checkpoints.CheckpointProvider,
	lastCheckpoint *configs.CheckpointExport,
	minimalCheckpoint int,
	fetchTimeout time.Duration,
) {
	S = New(denyListPath, providers, lastCheckpoint, minimalCheckpoint, fetchTimeout)
}

func (s *State) CurrentHeight() uint {
	ck := s.CurrentFirstCheckpoint()
	if ck == nil {
		return 0
	}
	h, err := strconv.ParseUint(ck.Checkpoint.Height, 10, 64)
	if err != nil {
		logs.Error.Printf("parse checkpoint height failed: %v", err)
	}
	return uint(h)
}

func (s *State) UpdateCheckpoints(height uint, hash string) error {
	s.Lock()
	defer s.Unlock()

	s.State.Store(int64(StatusSync))

	// Get checkpoints from the providers.
	ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
	defer cancel()
	cps, err := checkpoints.GetCheckpoints(ctx, s.providers, height, hash)
	if err != nil {
		return err
	}
	if len(cps) < s.minimalCheckpoint {
		return errors.New("not enough cps fetched")
	}

	inconsistent := checkpoints.Inconsistent(cps)
	if inconsistent {
		logs.Warn.Printf("Inconsistent cps: height=%d, hash=%s, starting verification and reconstruction...", height, hash)

		// Aggregate checkpoints by commitment.
		aggregates := make(map[string]*configs.CheckpointExport)
		for _, ck := range cps {
			if _, exist := aggregates[ck.Checkpoint.Commitment]; exist {
				continue
			}
			aggregates[ck.Checkpoint.Commitment] = ck
		}

		type succCommit struct {
			commitment  string
			transferLen int
		}

		succCommits := make(chan succCommit, len(aggregates))
		var wg sync.WaitGroup
		for commit, ck := range aggregates {
			wg.Add(1)
			go func(checkpointCommit string, ck *checkpoint.Checkpoint) {
				defer wg.Done()

				committeeCl, err := committee.New(ck.URL)
				if err != nil {
					logs.Error.Printf(
						"Ffailed to create committee indexer client: commit=%s, name=%s, url=%s, err=%v",
						checkpointCommit,
						ck.Name,
						ck.URL,
						err,
					)
					return
				}
				stateProof, err := committeeCl.LatestStateProof(context.Background())
				if err != nil {
					logs.Error.Printf(
						"Failed to get latest state proof from the committee indexer: commit=%s, name=%s, url=%s, err=%v",
						checkpointCommit,
						ck.Name,
						ck.URL,
						err,
					)
					return
				}
				if errMsg := stateProof.Error; errMsg != nil {
					logs.Error.Printf(
						"Latest state proof error from the committee indexer: commit=%s, name=%s, url=%s, err=%s",
						checkpointCommit,
						ck.Name,
						ck.URL,
						*errMsg,
					)
					return
				}

				// Verify Ordinals transfers via Bitcoin.
				var ordTransfers []getter.OrdTransfer
				for _, tran := range stateProof.Result.OrdTransfers {
					contentBytes, err := base64.StdEncoding.DecodeString(tran.Content)
					if err != nil {
						logs.Error.Printf(
							"Invalid Ordinals transfer content: commit=%s, name=%s, url=%s, err=%v",
							checkpointCommit,
							ck.Name,
							ck.URL,
							err,
						)
						return
					}
					ordTransfers = append(ordTransfers, getter.OrdTransfer{
						ID:            tran.ID,
						InscriptionID: tran.InscriptionID,
						OldSatpoint:   tran.OldSatpoint,
						NewSatpoint:   tran.NewSatpoint,
						NewPkscript:   tran.NewPkscript,
						NewWallet:     tran.NewWallet,
						SentAsFee:     tran.SentAsFee,
						Content:       contentBytes,
						ContentType:   tran.ContentType,
					})
				}

				curHeight, _ := strconv.ParseInt(ck.Height, 10, 64)
				ok, err := ordi.VerifyOrdTransfer(ordTransfers, uint(curHeight))
				if err != nil || !ok {
					logs.Error.Printf("Ordinals transfers verification error: err=%v, ok=%v", err, ok)
					return
				}

				preCheckpoint := s.lastCheckpoint
				prePointByte, err := base64.StdEncoding.DecodeString(preCheckpoint.Checkpoint.Commitment)
				if err != nil {
					return
				}
				prePoint := new(verkle.Point)
				if err := prePoint.SetBytes(prePointByte); err != nil {
					return
				}

				node, err := apis.GeneratePostRoot(prePoint, height, stateProof)
				if err != nil {
					logs.Error.Printf("generate post root error: %v", err)
					return
				}
				if node == nil {
					return
				}

				postBytes := node.Commit().Bytes()
				calCommit := base64.StdEncoding.EncodeToString(postBytes[:])
				if calCommit != checkpointCommit {
					logs.Warn.Printf(
						"inconsistent commits: calCommit=%s, checkpointCommit=%s",
						calCommit,
						checkpointCommit,
					)
					return
				}

				succCommits <- succCommit{
					commitment:  checkpointCommit,
					transferLen: len(ordTransfers),
				}
			}(commit, ck.Checkpoint)
		}
		wg.Wait()

		close(succCommits)
		var succVerify []succCommit
		for c := range succCommits {
			succVerify = append(succVerify, c)
		}
		if len(succVerify) == 0 {
			return errors.New("all cps verify failed")
		}

		maxTransfer := succVerify[0].transferLen
		champion := 0
		var seemRight []string
		for i := 1; i < len(succVerify); i++ {
			seemRight = append(seemRight, succVerify[i].commitment)
			if succVerify[i].transferLen > maxTransfer {
				maxTransfer = succVerify[i].transferLen
				champion = i
			}
		}
		trustCommitment := succVerify[champion].commitment

		s.lastCheckpoint, s.currentCheckpoints = s.currentCheckpoints[0], []*configs.CheckpointExport{aggregates[trustCommitment]}
		s.State.Store(int64(StatusActive))

		// Deny untrusted providers.
		for _, ck := range cps {
			if !slices.Contains(seemRight, ck.Checkpoint.Commitment) && s.denyListPath != "" {
				checkpoints.Deny(s.denyListPath, aggregates[trustCommitment], ck)
			}
		}
	} else {
		s.lastCheckpoint, s.currentCheckpoints = s.currentCheckpoints[0], cps
		s.State.Store(int64(StatusActive))
	}

	c := s.currentCheckpoints[0].Checkpoint.Commitment
	if inconsistent {
		logs.Info.Printf("Checkpoints fetched from providers have been verified, the commitment: %s, current height %d, hash %s", c, height, hash)
	} else {
		logs.Info.Printf("Checkpoints fetched from providers are consistent, the commitment: %s, current height %d, hash %s", c, height, hash)
	}

	return nil
}

func (s *State) LastCheckpoint() *configs.CheckpointExport {
	s.RLock()
	defer s.RUnlock()
	return s.lastCheckpoint
}

func (s *State) CurrentCheckpoints() []*configs.CheckpointExport {
	s.RLock()
	defer s.RUnlock()
	return s.currentCheckpoints
}

func (s *State) CurrentFirstCheckpoint() *configs.CheckpointExport {
	s.RLock()
	defer s.RUnlock()
	return s.currentCheckpoints[0]
}