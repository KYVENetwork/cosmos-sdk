package keeper

import (
	sdk "github.com/cosmos/cosmos-sdk/types"
	v1 "github.com/cosmos/cosmos-sdk/x/gov/types/v1"
	stakingtypes "github.com/cosmos/cosmos-sdk/x/staking/types"
)

// TODO: Break into several smaller functions for clarity

// NOTE: We have to check if the KYVE Protocol staking keeper is defined to ensure minimal changes.

// Tally iterates over the votes and updates the tally of a proposal based on the voting power of the
// voters
func (keeper Keeper) Tally(ctx sdk.Context, proposal v1.Proposal) (passes bool, burnDeposits bool, tallyResults v1.TallyResult) {
	results := make(map[v1.VoteOption]sdk.Dec)
	results[v1.OptionYes] = sdk.ZeroDec()
	results[v1.OptionAbstain] = sdk.ZeroDec()
	results[v1.OptionNo] = sdk.ZeroDec()
	results[v1.OptionNoWithVeto] = sdk.ZeroDec()

	totalVotingPower := sdk.ZeroDec()
	currValidators := make(map[string]v1.ValidatorGovInfo)

	// fetch all the bonded validators, insert them into currValidators
	keeper.sk.IterateBondedValidatorsByPower(ctx, func(index int64, validator stakingtypes.ValidatorI) (stop bool) {
		currValidators[validator.GetOperator().String()] = v1.NewValidatorGovInfo(
			validator.GetOperator(),
			validator.GetBondedTokens(),
			validator.GetDelegatorShares(),
			sdk.ZeroDec(),
			v1.WeightedVoteOptions{},
		)

		return false
	})

	// Fetch and insert all KYVE Protocol validators into list of current validators.
	// NOTE: The key used is a normal "kyve1blah" address.
	if keeper.protocolStakingKeeper != nil {
		for _, rawVal := range keeper.protocolStakingKeeper.GetActiveValidators(ctx) {
			// NOTE: We have to typecast to avoid creating import cycles when defining the function interfaces.
			if val, ok := rawVal.(v1.ValidatorGovInfo); ok {
				address := sdk.AccAddress(val.Address).String()
				currValidators[address] = val
			}
		}
	}

	keeper.IterateVotes(ctx, proposal.Id, func(vote v1.Vote) bool {
		// if validator, just record it in the map
		voter := sdk.MustAccAddressFromBech32(vote.Voter)

		valAddrStr := sdk.ValAddress(voter.Bytes()).String()
		if val, ok := currValidators[valAddrStr]; ok {
			val.Vote = vote.Options
			currValidators[valAddrStr] = val
		}
		// Check if the voter is a KYVE Protocol validator.
		if val, ok := currValidators[voter.String()]; ok {
			val.Vote = vote.Options
			currValidators[valAddrStr] = val
		}

		// iterate over all delegations from voter, deduct from any delegated-to validators
		keeper.sk.IterateDelegations(ctx, voter, func(index int64, delegation stakingtypes.DelegationI) (stop bool) {
			valAddrStr := delegation.GetValidatorAddr().String()

			if val, ok := currValidators[valAddrStr]; ok {
				// There is no need to handle the special case that validator address equal to voter address.
				// Because voter's voting power will tally again even if there will be deduction of voter's voting power from validator.
				val.DelegatorDeductions = val.DelegatorDeductions.Add(delegation.GetShares())
				currValidators[valAddrStr] = val

				// delegation shares * bonded / total shares
				votingPower := delegation.GetShares().MulInt(val.BondedTokens).Quo(val.DelegatorShares)

				for _, option := range vote.Options {
					weight, _ := sdk.NewDecFromStr(option.Weight)
					subPower := votingPower.Mul(weight)
					results[option.Option] = results[option.Option].Add(subPower)
				}
				totalVotingPower = totalVotingPower.Add(votingPower)
			}

			return false
		})

		if keeper.protocolStakingKeeper != nil {
			validators, amounts := keeper.protocolStakingKeeper.GetDelegations(ctx, voter.String())
			for idx, address := range validators {
				if val, ok := currValidators[address]; ok {
					val.DelegatorDeductions = val.DelegatorDeductions.Add(amounts[idx])
					currValidators[address] = val

					votingPower := amounts[idx].Quo(val.DelegatorShares)

					for _, option := range vote.Options {
						weight, _ := sdk.NewDecFromStr(option.Weight)
						subPower := votingPower.Mul(weight)
						results[option.Option] = results[option.Option].Add(subPower)
					}
					totalVotingPower = totalVotingPower.Add(votingPower)
				}
			}
		}

		keeper.deleteVote(ctx, vote.ProposalId, voter)
		return false
	})

	// iterate over the validators again to tally their voting power
	for _, val := range currValidators {
		if len(val.Vote) == 0 {
			continue
		}

		sharesAfterDeductions := val.DelegatorShares.Sub(val.DelegatorDeductions)
		votingPower := sharesAfterDeductions.MulInt(val.BondedTokens).Quo(val.DelegatorShares)

		for _, option := range val.Vote {
			weight, _ := sdk.NewDecFromStr(option.Weight)
			subPower := votingPower.Mul(weight)
			results[option.Option] = results[option.Option].Add(subPower)
		}
		totalVotingPower = totalVotingPower.Add(votingPower)
	}

	tallyParams := keeper.GetTallyParams(ctx)
	tallyResults = v1.NewTallyResultFromMap(results)

	totalBondedTokens := keeper.sk.TotalBondedTokens(ctx)
	if keeper.protocolStakingKeeper != nil {
		totalBondedTokens = totalBondedTokens.Add(
			keeper.protocolStakingKeeper.TotalBondedTokens(ctx),
		)
	}

	// TODO: Upgrade the spec to cover all of these cases & remove pseudocode.
	// If there is no staked coins, the proposal fails
	if totalBondedTokens.IsZero() {
		return false, false, tallyResults
	}

	// If there is not enough quorum of votes, the proposal fails
	percentVoting := totalVotingPower.Quo(sdk.NewDecFromInt(totalBondedTokens))
	quorum, _ := sdk.NewDecFromStr(tallyParams.Quorum)
	if percentVoting.LT(quorum) {
		return false, false, tallyResults
	}

	// If no one votes (everyone abstains), proposal fails
	if totalVotingPower.Sub(results[v1.OptionAbstain]).Equal(sdk.ZeroDec()) {
		return false, false, tallyResults
	}

	// If more than 1/3 of voters veto, proposal fails
	vetoThreshold, _ := sdk.NewDecFromStr(tallyParams.VetoThreshold)
	if results[v1.OptionNoWithVeto].Quo(totalVotingPower).GT(vetoThreshold) {
		return false, true, tallyResults
	}

	// If more than 1/2 of non-abstaining voters vote Yes, proposal passes
	threshold, _ := sdk.NewDecFromStr(tallyParams.Threshold)
	if results[v1.OptionYes].Quo(totalVotingPower.Sub(results[v1.OptionAbstain])).GT(threshold) {
		return true, false, tallyResults
	}

	// If more than 1/2 of non-abstaining voters vote No, proposal fails
	return false, false, tallyResults
}
