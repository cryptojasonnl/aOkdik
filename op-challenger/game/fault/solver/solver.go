package solver

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/ethereum-optimism/optimism/op-challenger/game/fault/types"
)

var (
	ErrStepNonLeafNode       = errors.New("cannot step on non-leaf claims")
	ErrStepAgreedClaim       = errors.New("cannot step on claims we agree with")
	ErrStepIgnoreInvalidPath = errors.New("cannot step on claims that dispute invalid paths")
)

// claimSolver uses a [TraceProvider] to determine the moves to make in a dispute game.
type claimSolver struct {
	trace     types.TraceAccessor
	gameDepth types.Depth
}

// newClaimSolver creates a new [claimSolver] using the provided [TraceProvider].
func newClaimSolver(gameDepth types.Depth, trace types.TraceAccessor) *claimSolver {
	return &claimSolver{
		trace,
		gameDepth,
	}
}

func (s *claimSolver) isSafeCounter(ctx context.Context, game types.Game, target types.Claim, pos types.Position) (bool, error) {
	honestTraceIdx := pos.TraceIndex(game.MaxDepth())
	claim := target
	// Find the claim with the highest trace index that is still strictly less than the honest move's trace index.
	var closestPrestateClaim types.Claim
	for {
		claimIdx := claim.TraceIndex(game.MaxDepth())
		if claimIdx.Cmp(honestTraceIdx) < 0 {
			if closestPrestateClaim == (types.Claim{}) || closestPrestateClaim.TraceIndex(game.MaxDepth()).Cmp(claimIdx) < 0 {
				closestPrestateClaim = claim
			}
		}
		if claim.IsRoot() {
			break
		}

		parent, err := game.GetParent(claim)
		if err != nil {
			return false, fmt.Errorf("failed to get parent of claim %v: %w", claim.ContractIndex, err)
		}
		claim = parent
	}
	if closestPrestateClaim == (types.Claim{}) {
		// No prestate, so safe to perform action
		return true, nil
	}
	valid, err := s.agreeWithClaim(ctx, game, closestPrestateClaim)
	if err != nil {
		return false, fmt.Errorf("failed to get correct claim at closest prestate: %w", err)
	}
	// Can only perform action if the possible prestate is valid.
	return valid, nil
}

// NextMove returns the next move to make given the current state of the game.
func (s *claimSolver) NextMove(ctx context.Context, claim types.Claim, game types.Game, agreedClaims *agreedClaimTracker) (*types.Claim, error) {
	if claim.Depth() == s.gameDepth {
		return nil, types.ErrGameDepthReached
	}

	if agreedClaims.IsAgreed(claim) {
		// Do not counter moves we would have made
		return nil, nil
	}

	agree, err := s.agreeWithClaim(ctx, game, claim)
	if err != nil {
		return nil, err
	}
	pos := claim.Position.Attack()
	if agree {
		pos = claim.Position.Defend()
	}
	safe, err := s.isSafeCounter(ctx, game, claim, pos)
	if err != nil {
		return nil, fmt.Errorf("failed to determine if move was safe: %w", err)
	}
	if !safe {
		return nil, nil
	}

	if agree {
		return s.defend(ctx, game, claim)
	} else {
		return s.attack(ctx, game, claim)
	}
}

type StepData struct {
	LeafClaim  types.Claim
	IsAttack   bool
	PreState   []byte
	ProofData  []byte
	OracleData *types.PreimageOracleData
}

// AttemptStep determines what step should occur for a given leaf claim.
// An error will be returned if the claim is not at the max depth.
// Returns ErrStepIgnoreInvalidPath if the claim disputes an invalid path
func (s *claimSolver) AttemptStep(ctx context.Context, game types.Game, claim types.Claim, agreedClaims *agreedClaimTracker) (*StepData, error) {
	if claim.Depth() != s.gameDepth {
		return nil, ErrStepNonLeafNode
	}

	if agreedClaims.IsAgreed(claim) {
		// Don't step on claims we would have made
		return nil, nil
	}

	claimCorrect, err := s.agreeWithClaim(ctx, game, claim)
	if err != nil {
		return nil, err
	}

	var position types.Position
	if !claimCorrect {
		// Attack the claim by executing step index, so we need to get the pre-state of that index
		position = claim.Position
	} else {
		// Defend and use this claim as the starting point to execute the step after.
		// Thus, we need the pre-state of the next step.
		position = claim.Position.MoveRight()
	}

	if safe, err := s.isSafeCounter(ctx, game, claim, position); err != nil {
		return nil, fmt.Errorf("failed to check if step was safe: %w", err)
	} else if !safe {
		// Do not try to step on claims with a poisoned prestate.
		return nil, nil
	}

	preState, proofData, oracleData, err := s.trace.GetStepData(ctx, game, claim, position)
	if err != nil {
		return nil, err
	}

	return &StepData{
		LeafClaim:  claim,
		IsAttack:   !claimCorrect,
		PreState:   preState,
		ProofData:  proofData,
		OracleData: oracleData,
	}, nil
}

// attack returns a response that attacks the claim.
func (s *claimSolver) attack(ctx context.Context, game types.Game, claim types.Claim) (*types.Claim, error) {
	position := claim.Attack()
	value, err := s.trace.Get(ctx, game, claim, position)
	if err != nil {
		return nil, fmt.Errorf("attack claim: %w", err)
	}
	return &types.Claim{
		ClaimData:           types.ClaimData{Value: value, Position: position},
		ParentContractIndex: claim.ContractIndex,
	}, nil
}

// defend returns a response that defends the claim.
func (s *claimSolver) defend(ctx context.Context, game types.Game, claim types.Claim) (*types.Claim, error) {
	if claim.IsRoot() {
		return nil, nil
	}
	position := claim.Defend()
	value, err := s.trace.Get(ctx, game, claim, position)
	if err != nil {
		return nil, fmt.Errorf("defend claim: %w", err)
	}
	return &types.Claim{
		ClaimData:           types.ClaimData{Value: value, Position: position},
		ParentContractIndex: claim.ContractIndex,
	}, nil
}

// agreeWithClaim returns true if the claim is correct according to the internal [TraceProvider].
func (s *claimSolver) agreeWithClaim(ctx context.Context, game types.Game, claim types.Claim) (bool, error) {
	ourValue, err := s.trace.Get(ctx, game, claim, claim.Position)
	return bytes.Equal(ourValue[:], claim.Value[:]), err
}
