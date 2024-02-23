package solver

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"math/big"
	"testing"

	faulttest "github.com/ethereum-optimism/optimism/op-challenger/game/fault/test"
	"github.com/ethereum-optimism/optimism/op-challenger/game/fault/trace"
	"github.com/ethereum-optimism/optimism/op-challenger/game/fault/types"
	gameTypes "github.com/ethereum-optimism/optimism/op-challenger/game/types"
	"github.com/ethereum-optimism/optimism/op-dispute-mon/mon/resolution"
	"github.com/ethereum-optimism/optimism/op-dispute-mon/mon/transform"
	"github.com/ethereum/go-ethereum/common"
	"github.com/stretchr/testify/require"
)

const expectFreeloaderCounters = false

type RunCondition uint8

const (
	RunAlways RunCondition = iota
	RunFreeloadersCountered
	RunFreeloadersNotCountered
)

var challengerAddr = common.Address(bytes.Repeat([]byte{0xaa}, 20))

func TestCalculateNextActions(t *testing.T) {
	maxDepth := types.Depth(6)
	startingL2BlockNumber := big.NewInt(0)
	claimBuilder := faulttest.NewAlphabetClaimBuilder(t, startingL2BlockNumber, maxDepth)

	tests := []struct {
		name             string
		rootClaimCorrect bool
		setupGame        func(builder *faulttest.GameBuilder)
		runCondition     RunCondition
	}{
		{
			name: "AttackRootClaim",
			setupGame: func(builder *faulttest.GameBuilder) {
				builder.Seq().ExpectAttack()
			},
		},
		{
			// Note: The fault dispute game contract should prevent a correct root claim from actually being posted
			// But for completeness, test we ignore it so we don't get sucked into playing an unwinnable game.
			name:             "DoNotAttackCorrectRootClaim_AgreeWithOutputRoot",
			rootClaimCorrect: true,
			setupGame:        func(builder *faulttest.GameBuilder) {},
		},
		{
			name: "DoNotPerformDuplicateMoves",
			setupGame: func(builder *faulttest.GameBuilder) {
				// Expected move has already been made.
				builder.Seq().AttackCorrect()
			},
		},
		{
			name: "RespondToAllClaimsAtDisagreeingLevel",
			setupGame: func(builder *faulttest.GameBuilder) {
				honestClaim := builder.Seq().AttackCorrect()
				honestClaim.AttackCorrect().ExpectDefend()
				honestClaim.DefendCorrect().ExpectDefend()
				honestClaim.Attack(common.Hash{0xaa}).ExpectAttack()
				honestClaim.Attack(common.Hash{0xbb}).ExpectAttack()
				honestClaim.Defend(common.Hash{0xcc}).ExpectAttack()
				honestClaim.Defend(common.Hash{0xdd}).ExpectAttack()
			},
		},
		{
			name: "StepAtMaxDepth",
			setupGame: func(builder *faulttest.GameBuilder) {
				lastHonestClaim := builder.Seq().
					AttackCorrect().
					AttackCorrect().
					DefendCorrect().
					DefendCorrect().
					DefendCorrect()
				lastHonestClaim.AttackCorrect().ExpectStepDefend()
				lastHonestClaim.Attack(common.Hash{0xdd}).ExpectStepAttack()
			},
		},
		{
			name: "PoisonedPreState",
			setupGame: func(builder *faulttest.GameBuilder) {
				// A claim hash that has no pre-image
				maliciousStateHash := common.Hash{0x01, 0xaa}

				// Dishonest actor counters their own claims to set up a situation with an invalid prestate
				// The honest actor should ignore path created by the dishonest actor, only supporting its own attack on the root claim
				honestMove := builder.Seq().AttackCorrect() // This expected action is the winning move.
				dishonestMove := honestMove.Attack(maliciousStateHash)
				// The expected action by the honest actor
				dishonestMove.ExpectAttack()
				// The honest actor will ignore this poisoned path
				dishonestMove.
					Defend(maliciousStateHash).
					Attack(maliciousStateHash)
			},
		},
		{
			name: "Freeloader-ValidClaimAtInvalidAttackPosition",
			setupGame: func(builder *faulttest.GameBuilder) {
				builder.Seq().
					AttackCorrect().                // Honest response to invalid root
					DefendCorrect().ExpectDefend(). // Defender agrees at this point, we should defend
					AttackCorrect().ExpectDefend()  // Freeloader attacks instead of defends
			},
			runCondition: RunFreeloadersCountered,
		},
		{
			name: "Freeloader-InvalidClaimAtInvalidAttackPosition",
			setupGame: func(builder *faulttest.GameBuilder) {
				builder.Seq().
					AttackCorrect().                         // Honest response to invalid root
					DefendCorrect().ExpectDefend().          // Defender agrees at this point, we should defend
					Attack(common.Hash{0xbb}).ExpectAttack() // Freeloader attacks with wrong claim instead of defends
			},
			runCondition: RunFreeloadersCountered,
		},
		{
			name: "Freeloader-InvalidClaimAtValidDefensePosition",
			setupGame: func(builder *faulttest.GameBuilder) {
				builder.Seq().
					AttackCorrect().                         // Honest response to invalid root
					DefendCorrect().ExpectDefend().          // Defender agrees at this point, we should defend
					Defend(common.Hash{0xbb}).ExpectAttack() // Freeloader defends with wrong claim, we should attack
			},
			runCondition: RunFreeloadersCountered,
		},
		{
			name: "Freeloader-InvalidClaimAtValidAttackPosition",
			setupGame: func(builder *faulttest.GameBuilder) {
				builder.Seq().
					AttackCorrect().                          // Honest response to invalid root
					Defend(common.Hash{0xaa}).ExpectAttack(). // Defender disagrees at this point, we should attack
					Attack(common.Hash{0xbb}).ExpectAttack()  // Freeloader attacks with wrong claim instead of defends
			},
			runCondition: RunFreeloadersCountered,
		},
		{
			name: "Freeloader-InvalidClaimAtInvalidDefensePosition",
			setupGame: func(builder *faulttest.GameBuilder) {
				builder.Seq().
					AttackCorrect().                          // Honest response to invalid root
					Defend(common.Hash{0xaa}).ExpectAttack(). // Defender disagrees at this point, we should attack
					Defend(common.Hash{0xbb})                 // Freeloader defends with wrong claim but we must not respond to avoid poisoning
			},
		},
		{
			name: "Freeloader-ValidClaimAtInvalidAttackPosition-RespondingToDishonestButCorrectAttack",
			setupGame: func(builder *faulttest.GameBuilder) {
				builder.Seq().
					AttackCorrect().                // Honest response to invalid root
					AttackCorrect().ExpectDefend(). // Defender attacks with correct value, we should defend
					AttackCorrect().ExpectDefend()  // Freeloader attacks with wrong claim, we should defend
			},
			runCondition: RunFreeloadersCountered,
		},
		{
			name: "Freeloader-DoNotCounterOwnClaim",
			setupGame: func(builder *faulttest.GameBuilder) {
				builder.Seq().
					AttackCorrect().                // Honest response to invalid root
					AttackCorrect().ExpectDefend(). // Defender attacks with correct value, we should defend
					AttackCorrect().                // Freeloader attacks instead, we should defend
					DefendCorrect()                 // We do defend and we shouldn't counter our own claim
			},
			runCondition: RunFreeloadersCountered,
		},
		{
			name: "Freeloader-ContinueDefendingAgainstFreeloader",
			setupGame: func(builder *faulttest.GameBuilder) {
				builder.Seq(). // invalid root
						AttackCorrect().                // Honest response to invalid root
						AttackCorrect().ExpectDefend(). // Defender attacks with correct value, we should defend
						AttackCorrect().                // Freeloader attacks instead, we should defend
						DefendCorrect().                // We do defend
						Attack(common.Hash{0xaa}).      // freeloader attacks our defense, we should attack
						ExpectAttack()
			},
			runCondition: RunFreeloadersCountered,
		},
		{
			name: "Freeloader-FreeloaderCountersRootClaim",
			setupGame: func(builder *faulttest.GameBuilder) {
				builder.Seq().
					ExpectAttack().            // Honest response to invalid root
					Attack(common.Hash{0xaa}). // freeloader
					ExpectAttack()             // Honest response to freeloader
			},
			runCondition: RunFreeloadersCountered,
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			enforceRunConditions(t, test.runCondition)
			builder := claimBuilder.GameBuilder(test.rootClaimCorrect)
			test.setupGame(builder)
			game := builder.Game
			logClaims(t, game)

			solver := NewGameSolver(maxDepth, trace.NewSimpleTraceAccessor(claimBuilder.CorrectTraceProvider()))
			postState, actions := runStep(t, solver, game, claimBuilder.CorrectTraceProvider())
			for i, action := range builder.ExpectedActions {
				t.Logf("Expect %v: Type: %v, ParentIdx: %v, Attack: %v, Value: %v, PreState: %v, ProofData: %v",
					i, action.Type, action.ParentIdx, action.IsAttack, action.Value, hex.EncodeToString(action.PreState), hex.EncodeToString(action.ProofData))
				require.Containsf(t, actions, action, "Expected claim %v missing", i)
			}
			require.Len(t, actions, len(builder.ExpectedActions), "Incorrect number of actions")

			verifyGameResolution(t, postState, test.rootClaimCorrect)
		})
	}
}

func runStep(t *testing.T, solver *GameSolver, game types.Game, correctTraceProvider types.TraceProvider) (types.Game, []types.Action) {
	actions, err := solver.CalculateNextActions(context.Background(), game)
	require.NoError(t, err)

	postState := applyActions(game, challengerAddr, actions)
	t.Log("Post state:")
	logClaims(t, postState)

	for i, action := range actions {
		t.Logf("Move %v: Type: %v, ParentIdx: %v, Attack: %v, Value: %v, PreState: %v, ProofData: %v",
			i, action.Type, action.ParentIdx, action.IsAttack, action.Value, hex.EncodeToString(action.PreState), hex.EncodeToString(action.ProofData))
		// Check that every move the solver returns meets the generic validation rules
		require.NoError(t, checkRules(game, action, correctTraceProvider), "Attempting to perform invalid action")
	}
	return postState, actions
}

func TestMultipleRounds(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name                string
		actor               actor
		runConditionValid   RunCondition
		runConditionInvalid RunCondition
	}{
		{
			name:  "SingleRoot",
			actor: doNothingActor,
		},
		{
			name:  "LinearAttackCorrect",
			actor: correctAttackLastClaim,
		},
		{
			name:  "LinearDefendCorrect",
			actor: correctDefendLastClaim,
		},
		{
			name:  "LinearAttackIncorrect",
			actor: incorrectAttackLastClaim,
		},
		{
			name:  "LinearDefendInorrect",
			actor: incorrectDefendLastClaim,
		},
		{
			name:  "LinearDefendIncorrectDefendCorrect",
			actor: combineActors(incorrectDefendLastClaim, correctDefendLastClaim),
		},
		{
			name:  "LinearAttackIncorrectDefendCorrect",
			actor: combineActors(incorrectAttackLastClaim, correctDefendLastClaim),
		},
		{
			name:  "LinearDefendIncorrectDefendIncorrect",
			actor: combineActors(incorrectDefendLastClaim, incorrectDefendLastClaim),
		},
		{
			name:  "LinearAttackIncorrectDefendIncorrect",
			actor: combineActors(incorrectAttackLastClaim, incorrectDefendLastClaim),
		},
		{
			name:  "AttackEverythingCorrect",
			actor: attackEverythingCorrect,
		},
		{
			name:  "DefendEverythingCorrect",
			actor: defendEverythingCorrect,
		},
		{
			name:  "AttackEverythingIncorrect",
			actor: attackEverythingIncorrect,
		},
		{
			name:  "DefendEverythingIncorrect",
			actor: defendEverythingIncorrect,
		},
		{
			name:  "Exhaustive",
			actor: exhaustive,
			// TODO(client-pod#611): We attempt to step even though the prestate is invalid
			// The step call would fail to estimate gas so not even send, but the challenger shouldn't try
			runConditionInvalid: RunFreeloadersCountered,
		},
	}
	for _, test := range tests {
		test := test
		for _, rootClaimCorrect := range []bool{true, false} {
			rootClaimCorrect := rootClaimCorrect
			t.Run(fmt.Sprintf("%v-%v", test.name, rootClaimCorrect), func(t *testing.T) {
				t.Parallel()
				runCondition := test.runConditionValid
				if !rootClaimCorrect {
					runCondition = test.runConditionInvalid
				}
				enforceRunConditions(t, runCondition)

				maxDepth := types.Depth(6)
				startingL2BlockNumber := big.NewInt(50)
				claimBuilder := faulttest.NewAlphabetClaimBuilder(t, startingL2BlockNumber, maxDepth)
				builder := claimBuilder.GameBuilder(rootClaimCorrect)
				game := builder.Game
				logClaims(t, game)

				correctTrace := claimBuilder.CorrectTraceProvider()
				solver := NewGameSolver(maxDepth, trace.NewSimpleTraceAccessor(correctTrace))

				roundNum := 0
				done := false
				for !done {
					t.Logf("------ ROUND %v ------", roundNum)
					game, _ = runStep(t, solver, game, correctTrace)
					verifyGameResolution(t, game, rootClaimCorrect)

					game, done = test.actor.Apply(t, game, correctTrace)
					roundNum++
				}
			})
		}
	}
}

func verifyGameResolution(t *testing.T, game types.Game, rootClaimCorrect bool) {
	actualResult, resolvedGame := gameResult(game)
	expectedResult := gameTypes.GameStatusChallengerWon
	if rootClaimCorrect {
		expectedResult = gameTypes.GameStatusDefenderWon
	}
	require.Equalf(t, expectedResult, actualResult, "Game should resolve correctly expected %v but was %v", expectedResult, actualResult)
	// Verify the challenger didn't have any of its bonds paid to someone else
	t.Log("Resolved game:")
	logClaims(t, resolvedGame)
	for _, claim := range resolvedGame.Claims() {
		if claim.Claimant != challengerAddr {
			continue
		}
		if claim.CounteredBy != (common.Address{}) {
			t.Fatalf("Challenger posted claim %v but it was countered by someone else:\n%v", claim.ContractIndex, printClaim(claim, game))
		}
	}
}

func logClaims(t *testing.T, game types.Game) {
	for _, claim := range game.Claims() {
		t.Log(printClaim(claim, game))
	}
}

func applyActions(game types.Game, claimant common.Address, actions []types.Action) types.Game {
	claims := game.Claims()
	for _, action := range actions {
		switch action.Type {
		case types.ActionTypeMove:
			newPosition := action.ParentPosition.Attack()
			if !action.IsAttack {
				newPosition = action.ParentPosition.Defend()
			}
			claim := types.Claim{
				ClaimData: types.ClaimData{
					Value:    action.Value,
					Bond:     big.NewInt(0),
					Position: newPosition,
				},
				Claimant:            claimant,
				Clock:               nil,
				ContractIndex:       len(claims),
				ParentContractIndex: action.ParentIdx,
			}
			claims = append(claims, claim)
		case types.ActionTypeStep:
			counteredClaim := claims[action.ParentIdx]
			counteredClaim.CounteredBy = claimant
			claims[action.ParentIdx] = counteredClaim
		default:
			panic(fmt.Errorf("unknown move type: %v", action.Type))
		}
	}
	return types.NewGameState(claims, game.MaxDepth())
}

func gameResult(game types.Game) (gameTypes.GameStatus, types.Game) {
	tree := transform.CreateBidirectionalTree(game.Claims())
	result := resolution.Resolve(tree)
	resolvedClaims := make([]types.Claim, 0, len(tree.Claims))
	for _, claim := range tree.Claims {
		resolvedClaims = append(resolvedClaims, *claim.Claim)
	}
	return result, types.NewGameState(resolvedClaims, game.MaxDepth())
}

func enforceRunConditions(t *testing.T, runCondition RunCondition) {
	switch runCondition {
	case RunAlways:
	case RunFreeloadersCountered:
		if !expectFreeloaderCounters {
			t.Skip("Freeloader countering not enabled")
		}
	case RunFreeloadersNotCountered:
		if expectFreeloaderCounters {
			t.Skip("Freeloader countering enabled")
		}
	}
}
