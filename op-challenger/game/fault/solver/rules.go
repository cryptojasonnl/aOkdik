package solver

import (
	"context"
	"errors"
	"fmt"
	"math/big"

	"github.com/ethereum-optimism/optimism/op-challenger/game/fault/types"
)

type actionRule func(game types.Game, action types.Action, correctTrace types.TraceProvider) error

var rules = []actionRule{
	parentMustExist,
	onlyStepAtMaxDepth,
	onlyMoveBeforeMaxDepth,
	doNotDuplicateExistingMoves,
	doNotDefendRootClaim,
	avoidPoisonedPrestate,
	detectPoisonedStepPrestate,
	detectFailedStep,
}

func printClaim(claim types.Claim, game types.Game) string {
	return fmt.Sprintf("Claim %v: Pos: %v TraceIdx: %v Depth: %v IndexAtDepth: %v ParentIdx: %v Value: %v Claimant: %v CounteredBy: %v",
		claim.ContractIndex, claim.Position.ToGIndex(), claim.Position.TraceIndex(game.MaxDepth()), claim.Position.Depth(), claim.Position.IndexAtDepth(), claim.ParentContractIndex, claim.Value, claim.Claimant, claim.CounteredBy)
}

func checkRules(game types.Game, action types.Action, correctTrace types.TraceProvider) error {
	var errs []error
	for _, rule := range rules {
		errs = append(errs, rule(game, action, correctTrace))
	}
	return errors.Join(errs...)
}

func parentMustExist(game types.Game, action types.Action, correctTrace types.TraceProvider) error {
	if len(game.Claims()) <= action.ParentIdx || action.ParentIdx < 0 {
		return fmt.Errorf("parent claim %v does not exist in game with %v claims", action.ParentIdx, len(game.Claims()))
	}
	return nil
}

func onlyStepAtMaxDepth(game types.Game, action types.Action, correctTrace types.TraceProvider) error {
	if action.Type == types.ActionTypeStep {
		return nil
	}
	parentDepth := game.Claims()[action.ParentIdx].Position.Depth()
	if parentDepth >= game.MaxDepth() {
		return fmt.Errorf("parent at max depth (%v) but attempting to perform %v action instead of step",
			parentDepth, action.Type)
	}
	return nil
}

func onlyMoveBeforeMaxDepth(game types.Game, action types.Action, correctTrace types.TraceProvider) error {
	if action.Type == types.ActionTypeMove {
		return nil
	}
	parentDepth := game.Claims()[action.ParentIdx].Position.Depth()
	if parentDepth < game.MaxDepth() {
		return fmt.Errorf("parent (%v) not at max depth (%v) but attempting to perform %v action instead of move",
			parentDepth, game.MaxDepth(), action.Type)
	}
	return nil
}

func doNotDuplicateExistingMoves(game types.Game, action types.Action, correctTrace types.TraceProvider) error {
	newClaimData := types.ClaimData{
		Value:    action.Value,
		Position: resultingPosition(game, action),
	}
	if _, dupe := game.IsDuplicate(types.Claim{ClaimData: newClaimData, ParentContractIndex: action.ParentIdx}); dupe {
		return fmt.Errorf("creating duplicate claim at %v with value %v", newClaimData.Position.ToGIndex(), newClaimData.Value)
	}
	return nil
}

func doNotDefendRootClaim(game types.Game, action types.Action, correctTrace types.TraceProvider) error {
	if game.Claims()[action.ParentIdx].IsRootPosition() && !action.IsAttack {
		return fmt.Errorf("defending the root claim at idx %v", action.ParentIdx)
	}
	return nil
}

func avoidPoisonedPrestate(game types.Game, action types.Action, correctTrace types.TraceProvider) error {
	if action.Type == types.ActionTypeStep {
		return nil
	}
	ancestors := ""
	movePosition := resultingPosition(game, action)
	honestTraceIndex := movePosition.TraceIndex(game.MaxDepth())
	// Walk back up the claims and find the claim with highest trace index < honestTraceIndex
	claim := game.Claims()[action.ParentIdx]
	var preStateClaim types.Claim
	for {
		ancestors += printClaim(claim, game) + "\n"
		claimTraceIdx := claim.TraceIndex(game.MaxDepth())
		if claimTraceIdx.Cmp(honestTraceIndex) < 0 { // Check it's left of the honest claim
			if preStateClaim == (types.Claim{}) || claimTraceIdx.Cmp(preStateClaim.TraceIndex(game.MaxDepth())) > 0 {
				preStateClaim = claim
			}
		}
		if claim.IsRoot() {
			break
		}
		parent, err := game.GetParent(claim)
		if err != nil {
			return fmt.Errorf("no parent of claim %v: %w", claim.ContractIndex, err)
		}
		claim = parent
	}
	if preStateClaim == (types.Claim{}) {
		// No claim to the left of the honest claim, so can't have been poisoned
		return nil
	}
	correctValue, err := correctTrace.Get(context.Background(), preStateClaim.Position)
	if err != nil {
		return fmt.Errorf("failed to get correct trace at position %v: %w", preStateClaim.Position, err)
	}
	if correctValue != preStateClaim.Value {
		err = fmt.Errorf("prestate poisoned claim %v has invalid prestate and is left of honest claim countering %v at trace index %v", preStateClaim.ContractIndex, action.ParentIdx, honestTraceIndex)
		return err
	}
	return nil
}

// detectFailedStep checks that step actions will succeed.
//
// INVARIANT: If a step is an attack, the poststate is valid if the step produces
//
//	the same poststate hash as the parent claim's value.
//	If a step is a defense:
//	  1. If the parent claim and the found post state agree with each other
//	     (depth diff % 2 == 0), the step is valid if it produces the same
//	     state hash as the post state's claim.
//	  2. If the parent claim and the found post state disagree with each other
//	     (depth diff % 2 != 0), the parent cannot be countered unless the step
//	     produces the same state hash as `postState.claim`.
func detectFailedStep(game types.Game, action types.Action, correctTrace types.TraceProvider) error {
	if action.Type != types.ActionTypeStep {
		// An invalid post state is not an issue if we are moving, only if the honest challenger has to call step.
		return nil
	}
	position := resultingPosition(game, action)
	if position.Depth() != game.MaxDepth() {
		// Not at max depth yet
		return nil
	}
	honestTraceIndex := position.TraceIndex(game.MaxDepth())
	poststateIndex := honestTraceIndex
	if !action.IsAttack {
		poststateIndex = new(big.Int).Add(honestTraceIndex, big.NewInt(1))
	}
	// Walk back up the claims and find the claim required post state index
	claim := game.Claims()[action.ParentIdx]
	poststateClaim, ok := game.AncestorWithTraceIndex(claim, poststateIndex)
	if !ok {
		return fmt.Errorf("did not find required poststate at %v to counter claim %v", poststateIndex, action.ParentIdx)
	}
	correctValue, err := correctTrace.Get(context.Background(), poststateClaim.Position)
	if err != nil {
		return fmt.Errorf("failed to get correct trace at position %v: %w", poststateClaim.Position, err)
	}
	validStep := correctValue == poststateClaim.Value
	parentPostAgree := (claim.Depth()-poststateClaim.Depth())%2 == 0
	if parentPostAgree == validStep {
		return fmt.Errorf("failed step against claim at %v using poststate from claim %v post state is correct? %v parentPostAgree? %v",
			action.ParentIdx, poststateClaim.ContractIndex, validStep, parentPostAgree)
	}
	return nil
}

func detectPoisonedStepPrestate(game types.Game, action types.Action, correctTrace types.TraceProvider) error {
	position := resultingPosition(game, action)
	if position.Depth() != game.MaxDepth() {
		// Not at max depth yet
		return nil
	}
	honestTraceIndex := position.TraceIndex(game.MaxDepth())
	prestateIndex := honestTraceIndex
	// If we're performing a move to post a leaf claim, assume the attacker will try to attack it from their
	// poisoned prestate
	if action.IsAttack || action.Type == types.ActionTypeMove {
		prestateIndex = new(big.Int).Sub(prestateIndex, big.NewInt(1))
	}
	if prestateIndex.Cmp(big.NewInt(0)) < 0 {
		// Absolute prestate is not poisoned
		return nil
	}
	// Walk back up the claims and find the claim with highest trace index < honestTraceIndex
	claim := game.Claims()[action.ParentIdx]
	preStateClaim, ok := game.AncestorWithTraceIndex(claim, prestateIndex)
	if !ok {
		return fmt.Errorf("performing step against claim %v with no prestate available at %v", claim.ContractIndex, prestateIndex)
	}
	correctValue, err := correctTrace.Get(context.Background(), preStateClaim.Position)
	if err != nil {
		return fmt.Errorf("failed to get correct trace at position %v: %w", preStateClaim.Position, err)
	}
	if correctValue != preStateClaim.Value {
		if action.Type == types.ActionTypeStep {
			return fmt.Errorf("stepping from poisoned prestate at claim %v when countering %v", preStateClaim.ContractIndex, action.ParentIdx)
		} else {
			return fmt.Errorf("posting leaf claim with poisoned prestate from claim %v when countering %v", preStateClaim.ContractIndex, action.ParentIdx)
		}
	}
	return nil
}

func resultingPosition(game types.Game, action types.Action) types.Position {
	parentPos := game.Claims()[action.ParentIdx].Position
	if action.Type == types.ActionTypeStep {
		return parentPos
	}
	if action.IsAttack {
		return parentPos.Attack()
	}
	return parentPos.Defend()
}
