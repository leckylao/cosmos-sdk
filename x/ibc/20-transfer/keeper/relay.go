package keeper

import (
	"strings"

	sdk "github.com/cosmos/cosmos-sdk/types"
	sdkerrors "github.com/cosmos/cosmos-sdk/types/errors"
	channel "github.com/cosmos/cosmos-sdk/x/ibc/04-channel/types"
	"github.com/cosmos/cosmos-sdk/x/ibc/20-transfer/types"
	ibctypes "github.com/cosmos/cosmos-sdk/x/ibc/types"
)

// SendTransfer handles transfer sending logic. There are 2 possible cases:
//
// 1. Sender chain is the source chain of the coins (i.e where they were minted): the coins
// are transferred to an escrow address (i.e locked) on the sender chain and then
// transferred to the destination chain (i.e not the source chain) via a packet
// with the corresponding fungible token data.
//
// 2. Coins are not native from the sender chain (i.e tokens sent where transferred over
// through IBC already): the coins are burned and then a packet is sent to the
// source chain of the tokens.
func (k Keeper) SendTransfer(
	ctx sdk.Context,
	sourcePort,
	sourceChannel string,
	destHeight uint64,
	amount sdk.Coins,
	sender,
	receiver sdk.AccAddress,
) error {
	sourceChannelEnd, found := k.channelKeeper.GetChannel(ctx, sourcePort, sourceChannel)
	if !found {
		return sdkerrors.Wrap(channel.ErrChannelNotFound, sourceChannel)
	}

	destinationPort := sourceChannelEnd.Counterparty.PortID
	destinationChannel := sourceChannelEnd.Counterparty.ChannelID

	// get the next sequence
	sequence, found := k.channelKeeper.GetNextSequenceSend(ctx, sourcePort, sourceChannel)
	if !found {
		return channel.ErrSequenceSendNotFound
	}

	return k.createOutgoingPacket(ctx, sequence, sourcePort, sourceChannel, destinationPort, destinationChannel, destHeight, amount, sender, receiver)
}

// See spec for this function: https://github.com/cosmos/ics/tree/master/spec/ics-020-fungible-token-transfer#packet-relay
func (k Keeper) createOutgoingPacket(
	ctx sdk.Context,
	seq uint64,
	sourcePort, sourceChannel,
	destinationPort, destinationChannel string,
	destHeight uint64,
	amount sdk.Coins,
	sender, receiver sdk.AccAddress,
) error {
	channelCap, ok := k.scopedKeeper.GetCapability(ctx, ibctypes.ChannelCapabilityPath(sourcePort, sourceChannel))
	if !ok {
		return sdkerrors.Wrap(channel.ErrChannelCapabilityNotFound, "module does not own channel capability")
	}
	// NOTE:
	// - Coins transferred from the destination chain should have their denomination
	// prefixed with source port and channel IDs.
	// - Coins transferred from the source chain can have their denomination
	// clear from prefixes when transferred to the escrow account (i.e when they are
	// locked) BUT MUST have the destination port and channel ID when constructing
	// the packet data.
	if len(amount) != 1 {
		return sdkerrors.Wrapf(types.ErrOnlyOneDenomAllowed, "%d denoms included", len(amount))
	}

	prefix := types.GetDenomPrefix(destinationPort, destinationChannel)
	source := strings.HasPrefix(amount[0].Denom, prefix)

	if source {
		// clear the denomination from the prefix to send the coins to the escrow account
		coins := make(sdk.Coins, len(amount))
		for i, coin := range amount {
			if strings.HasPrefix(coin.Denom, prefix) {
				coins[i] = sdk.NewCoin(coin.Denom[len(prefix):], coin.Amount)
			} else {
				coins[i] = coin
			}
		}

		// escrow tokens if the destination chain is the same as the sender's
		escrowAddress := types.GetEscrowAddress(sourcePort, sourceChannel)

		// escrow source tokens. It fails if balance insufficient.
		if err := k.bankKeeper.SendCoins(
			ctx, sender, escrowAddress, coins,
		); err != nil {
			return err
		}

	} else {
		// build the receiving denomination prefix if it's not present
		prefix = types.GetDenomPrefix(sourcePort, sourceChannel)
		for _, coin := range amount {
			if !strings.HasPrefix(coin.Denom, prefix) {
				return sdkerrors.Wrapf(types.ErrInvalidDenomForTransfer, "denom was: %s", coin.Denom)
			}
		}

		// transfer the coins to the module account and burn them
		if err := k.supplyKeeper.SendCoinsFromAccountToModule(
			ctx, sender, types.GetModuleAccountName(), amount,
		); err != nil {
			return err
		}

		// burn vouchers from the sender's balance if the source is from another chain
		if err := k.supplyKeeper.BurnCoins(
			ctx, types.GetModuleAccountName(), amount,
		); err != nil {
			// NOTE: should not happen as the module account was
			// retrieved on the step above and it has enough balace
			// to burn.
			return err
		}
	}

	packetData := types.NewFungibleTokenPacketData(
		amount, sender, receiver,
	)

	packet := channel.NewPacket(
		packetData.GetBytes(),
		seq,
		sourcePort,
		sourceChannel,
		destinationPort,
		destinationChannel,
		destHeight+DefaultPacketTimeout,
	)

	return k.channelKeeper.SendPacket(ctx, channelCap, packet)
}

func (k Keeper) OnRecvPacket(ctx sdk.Context, packet channel.Packet, data types.FungibleTokenPacketData) error {
	// NOTE: packet data type already checked in handler.go

	if len(data.Amount) != 1 {
		return sdkerrors.Wrapf(types.ErrOnlyOneDenomAllowed, "%d denoms included", len(data.Amount))
	}

	prefix := types.GetDenomPrefix(packet.GetDestPort(), packet.GetDestChannel())
	source := strings.HasPrefix(data.Amount[0].Denom, prefix)

	if source {
		// mint new tokens if the source of the transfer is the same chain
		if err := k.supplyKeeper.MintCoins(
			ctx, types.GetModuleAccountName(), data.Amount,
		); err != nil {
			return err
		}

		// send to receiver
		return k.supplyKeeper.SendCoinsFromModuleToAccount(
			ctx, types.GetModuleAccountName(), data.Receiver, data.Amount,
		)
	}

	// check the denom prefix
	prefix = types.GetDenomPrefix(packet.GetSourcePort(), packet.GetSourceChannel())
	coins := make(sdk.Coins, len(data.Amount))
	for i, coin := range data.Amount {
		if !strings.HasPrefix(coin.Denom, prefix) {
			return sdkerrors.Wrapf(
				sdkerrors.ErrInvalidCoins,
				"%s doesn't contain the prefix '%s'", coin.Denom, prefix,
			)
		}
		coins[i] = sdk.NewCoin(coin.Denom[len(prefix):], coin.Amount)
	}

	// unescrow tokens
	escrowAddress := types.GetEscrowAddress(packet.GetDestPort(), packet.GetDestChannel())
	return k.bankKeeper.SendCoins(ctx, escrowAddress, data.Receiver, coins)
}

func (k Keeper) OnAcknowledgementPacket(ctx sdk.Context, packet channel.Packet, data types.FungibleTokenPacketData, ack types.FungibleTokenPacketAcknowledgement) error {
	if !ack.Success {
		return k.refundPacketAmount(ctx, packet, data)
	}
	return nil
}

func (k Keeper) OnTimeoutPacket(ctx sdk.Context, packet channel.Packet, data types.FungibleTokenPacketData) error {
	return k.refundPacketAmount(ctx, packet, data)
}

func (k Keeper) refundPacketAmount(ctx sdk.Context, packet channel.Packet, data types.FungibleTokenPacketData) error {
	// NOTE: packet data type already checked in handler.go

	if len(data.Amount) != 1 {
		return sdkerrors.Wrapf(types.ErrOnlyOneDenomAllowed, "%d denoms included", len(data.Amount))
	}

	// check the denom prefix
	prefix := types.GetDenomPrefix(packet.GetSourcePort(), packet.GetSourceChannel())
	source := strings.HasPrefix(data.Amount[0].Denom, prefix)

	if source {
		coins := make(sdk.Coins, len(data.Amount))
		for i, coin := range data.Amount {
			coin := coin
			if !strings.HasPrefix(coin.Denom, prefix) {
				return sdkerrors.Wrapf(sdkerrors.ErrInvalidCoins, "%s doesn't contain the prefix '%s'", coin.Denom, prefix)
			}
			coins[i] = sdk.NewCoin(coin.Denom[len(prefix):], coin.Amount)
		}

		// unescrow tokens back to sender
		escrowAddress := types.GetEscrowAddress(packet.GetDestPort(), packet.GetDestChannel())
		return k.bankKeeper.SendCoins(ctx, escrowAddress, data.Sender, coins)
	}

	// mint vouchers back to sender
	if err := k.supplyKeeper.MintCoins(
		ctx, types.GetModuleAccountName(), data.Amount,
	); err != nil {
		return err
	}

	return k.supplyKeeper.SendCoinsFromModuleToAccount(ctx, types.GetModuleAccountName(), data.Sender, data.Amount)
}
