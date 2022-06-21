package itest

import (
	"context"
	"fmt"
	"io/ioutil"
	"testing"

	"github.com/btcsuite/btcutil"
	"github.com/lightninglabs/lightning-terminal/accounts"
	"github.com/lightninglabs/lightning-terminal/litrpc"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/macaroons"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"gopkg.in/macaroon.v2"
)

var (
	burnAddr = "bcrt1qlthqw0zmup27nx35hcy82vkc4qjcxgmkvhnjtc"
)

// runAccountSystemTest tests the macaroon account system.
func runAccountSystemTest(t *harnessTest, node *HarnessNode, hostPort,
	tlsCertPath, macPath string, runNumber int) {

	net := t.lndHarness
	ctxb := context.Background()
	ctxt, cancel := context.WithTimeout(ctxb, defaultTimeout)
	defer cancel()

	// Before we start opening channels, we want to make sure we don't have
	// any leftover funds in the tested node's wallet, so we can always
	// exactly calculate what we are supposed to have during our test.
	_, err := node.LightningClient.SendCoins(ctxt, &lnrpc.SendCoinsRequest{
		Addr:             burnAddr,
		SendAll:          true,
		MinConfs:         0,
		SpendUnconfirmed: true,
	})
	mineBlocks(t, net, 1, 1)

	// Set up our channel partner Charlie that is being used to open
	// channels to, send and receive payments to verify the responses of the
	// different RPCs.
	charlie, err := net.NewNode("Charlie", nil, false, true)
	require.NoError(t.t, err)
	defer shutdownAndAssert(net, t, charlie)

	const (
		initialBalance = btcutil.SatoshiPerBitcoin
		fundingAmt     = 5_000_000
		pushAmt        = 2_000_000
		chanReserve    = 9050
		chainFees      = 7637
	)

	net.SendCoins(t.t, initialBalance, node)
	net.EnsureConnected(t.t, node, charlie)

	channelOp := openChannelAndAssert(
		t, net, node, charlie, lntest.OpenChannelParams{
			Amt:     fundingAmt,
			PushAmt: pushAmt,
		},
	)

	// Make sure our normal calls all return the expected values.
	localBalance := uint64(fundingAmt - pushAmt - chanReserve)
	assertChannelBalance(
		ctxt, t.t, node.LightningClient, localBalance, pushAmt,
	)
	walletBalance := int64(initialBalance - fundingAmt - chainFees)
	assertWalletBalance(
		ctxt, t.t, node.LightningClient, walletBalance, 0,
		walletBalance, 0,
	)
	assertNumChannels(ctxt, t.t, node.LightningClient, 1, 0, 0, 0)
	assertNumPeers(ctxt, t.t, node.LightningClient, 2)

	// Prepare our gRPC connection with the super macaroon as the
	// authentication mechanism.
	rawConn, err := connectRPC(ctxt, hostPort, tlsCertPath)
	require.NoError(t.t, err)

	macBytes, err := ioutil.ReadFile(macPath)
	require.NoError(t.t, err)
	ctxm := macaroonContext(ctxt, macBytes)
	acctClient := litrpc.NewAccountsClient(rawConn)

	// Create a new account with a balance of 50k sats.
	const acctBalance uint64 = 50_000
	acctResp, err := acctClient.CreateAccount(
		ctxm, &litrpc.CreateAccountRequest{
			AccountBalance: acctBalance,
		},
	)
	require.NoError(t.t, err)
	require.NotNil(t.t, acctResp.Account)
	require.Greater(t.t, len(acctResp.Account.Id), 12)
	require.Equal(t.t, acctBalance, acctResp.Account.CurrentBalance)
	require.Equal(t.t, acctBalance, acctResp.Account.InitialBalance)

	// Now create a new macaroon that has the account caveat attached to it.
	var acctMacaroon macaroon.Macaroon
	require.NoError(t.t, acctMacaroon.UnmarshalBinary(macBytes))
	err = acctMacaroon.AddFirstPartyCaveat([]byte(fmt.Sprintf(
		"%s %s %s", macaroons.CondLndCustom, accounts.CondAccount,
		acctResp.Account.Id,
	)))
	require.NoError(t.t, err)

	// Serialize the constrained macaroon and prepare a context that we can
	// use to make requests.
	acctMacBytes, err := acctMacaroon.MarshalBinary()
	require.NoError(t.t, err)
	ctxa := macaroonContext(ctxt, acctMacBytes)

	// We now create a few invoices with the "admin" connection and also pay
	// a few invoices created by our helper node Charlie. Both the invoices
	// and the payments should not show up in the responses of the account
	// RPC calls as they don't belong to the account.
	_, err = node.AddInvoice(ctxt, &lnrpc.Invoice{
		Value: 1234,
		Memo:  "admin",
	})
	require.NoError(t.t, err)
	_, err = node.AddInvoice(ctxt, &lnrpc.Invoice{
		Value: 3456,
		Memo:  "admin",
	})
	require.NoError(t.t, err)

	// We can't delete invoices, so there will be residual invoices from
	// previous runs on the same node.
	assertNumInvoices(
		ctxt, t.t, node.LightningClient, runNumber*2+(runNumber-1),
	)

	payNode(ctxt, ctxt, t, node.RouterClient, charlie, 4567, "invoice 1")
	payNode(ctxt, ctxt, t, node.RouterClient, charlie, 2345, "invoice 2")
	assertNumPayments(ctxt, t.t, node.LightningClient, 2)

	// Run the actual account restriction tests against the connection with
	// the account macaroon.
	testAccountRestrictions(ctxa, t, net, rawConn, charlie, acctBalance)

	// Clean up our channel and payments, so we can start the next test
	// iteration with a clean slate.
	closeChannelAndAssert(t, net, node, channelOp, false)

	_, err = node.DeleteAllPayments(ctxt, &lnrpc.DeleteAllPaymentsRequest{
		FailedPaymentsOnly: false,
		FailedHtlcsOnly:    false,
	})
	require.NoError(t.t, err)
}

// testAccountRestrictions tests the different scenarios in which the account
// restricted RPC responses differ from the normal responses.
func testAccountRestrictions(ctxa context.Context, t *harnessTest,
	net *NetworkHarness, rawConn *grpc.ClientConn, charlie *HarnessNode,
	initialAccountBalance uint64) {

	// The ctxa variable is the context with the restricted account macaroon
	// applied to it. But we also need a timeout context for things we do
	// with the charlie node.
	ctxb := context.Background()
	ctxt, cancel := context.WithTimeout(ctxb, defaultTimeout)
	defer cancel()

	// Let's do some basic validation calls against the lnrpc interface.
	lightningClient := lnrpc.NewLightningClient(rawConn)
	routerClient := routerrpc.NewRouterClient(rawConn)

	// The channel balance should always reflect our account balance.
	assertChannelBalance(
		ctxa, t.t, lightningClient, initialAccountBalance, 0,
	)

	// The on-chain balance should always be zero, no on-chain transactions
	// should show up and nothing channel or peer related should be shown.
	assertWalletBalance(ctxa, t.t, lightningClient, 0, 0, 0, 0)
	assertNumChannels(ctxa, t.t, lightningClient, 0, 0, 0, 0)
	assertNumPeers(ctxa, t.t, lightningClient, 0)

	txnsResp, err := lightningClient.GetTransactions(
		ctxa, &lnrpc.GetTransactionsRequest{},
	)
	require.NoError(t.t, err)
	require.Len(t.t, txnsResp.Transactions, 0)

	// There should be no invoices or payments since they were made with the
	// "admin" macaroon.
	assertNumInvoices(ctxa, t.t, lightningClient, 0)
	assertNumPayments(ctxa, t.t, lightningClient, 0)

	// Let's now create an invoice with the account macaroon, so we can pay
	// it to increase the account balance.
	const inboundPaymentAmt = 7777
	payNode(
		ctxa, ctxt, t, charlie.RouterClient, lightningClient,
		inboundPaymentAmt, "plz ser my account so poor",
	)
	assertNumInvoices(ctxa, t.t, lightningClient, 1)
	assertNumPayments(ctxa, t.t, lightningClient, 0)
	assertChannelBalance(
		ctxa, t.t, lightningClient,
		initialAccountBalance+inboundPaymentAmt, 0,
	)

	// Great, now let's also test that we can pay an invoice from the
	// account which will deduct the amount from the account balance.
	const outboundPaymentAmt = 4444
	payNode(
		ctxt, ctxa, t, routerClient, charlie, outboundPaymentAmt,
		"yo, watch this",
	)
	assertNumInvoices(ctxa, t.t, lightningClient, 1)
	assertNumPayments(ctxa, t.t, lightningClient, 1)
	assertChannelBalance(
		ctxa, t.t, lightningClient,
		initialAccountBalance+inboundPaymentAmt-outboundPaymentAmt, 0,
	)
}

func assertChannelBalance(ctx context.Context, t *testing.T,
	client lnrpc.LightningClient, localBalance, remoteBalance uint64) {

	channelBalanceResp, err := client.ChannelBalance(
		ctx, &lnrpc.ChannelBalanceRequest{},
	)
	require.NoError(t, err)
	require.Equal(
		t, int(localBalance),
		int(channelBalanceResp.LocalBalance.Sat),
	)
	require.Equal(
		t, int(remoteBalance),
		int(channelBalanceResp.RemoteBalance.Sat),
	)
}

func assertWalletBalance(ctx context.Context, t *testing.T,
	client lnrpc.LightningClient, totalBalance, lockedBalance,
	confirmedBalance, unconfirmedBalance int64) {

	walletBalanceResp, err := client.WalletBalance(
		ctx, &lnrpc.WalletBalanceRequest{},
	)
	require.NoError(t, err)
	require.Equal(t, totalBalance, walletBalanceResp.TotalBalance)
	require.Equal(t, lockedBalance, walletBalanceResp.LockedBalance)
	require.Equal(t, confirmedBalance, walletBalanceResp.ConfirmedBalance)
	require.Equal(
		t, unconfirmedBalance, walletBalanceResp.UnconfirmedBalance,
	)
}

func assertNumChannels(ctx context.Context, t *testing.T,
	client lnrpc.LightningClient, numActive, numPendingOpen,
	numPendingForceClosing, numWaitingClose int) {

	listChannelsResp, err := client.ListChannels(
		ctx, &lnrpc.ListChannelsRequest{},
	)
	require.NoError(t, err)
	require.Len(t, listChannelsResp.Channels, numActive)

	pendingChannelsResp, err := client.PendingChannels(
		ctx, &lnrpc.PendingChannelsRequest{},
	)
	require.NoError(t, err)
	require.Len(t, pendingChannelsResp.PendingOpenChannels, numPendingOpen)
	require.Len(
		t, pendingChannelsResp.PendingForceClosingChannels,
		numPendingForceClosing,
	)
	require.Len(
		t, pendingChannelsResp.WaitingCloseChannels, numWaitingClose,
	)
}

func assertNumPeers(ctx context.Context, t *testing.T,
	client lnrpc.LightningClient, numPeers int) {

	listPeersResp, err := client.ListPeers(
		ctx, &lnrpc.ListPeersRequest{},
	)
	require.NoError(t, err)
	require.Len(t, listPeersResp.Peers, numPeers)
}

func assertNumInvoices(ctx context.Context, t *testing.T,
	client lnrpc.LightningClient, numInvoices int) {

	listInvoicesResp, err := client.ListInvoices(
		ctx, &lnrpc.ListInvoiceRequest{},
	)
	require.NoError(t, err)
	require.Len(t, listInvoicesResp.Invoices, numInvoices)
}

func assertNumPayments(ctx context.Context, t *testing.T,
	client lnrpc.LightningClient, numPayments int) {

	listPaymentsResp, err := client.ListPayments(
		ctx, &lnrpc.ListPaymentsRequest{IncludeIncomplete: true},
	)
	require.NoError(t, err)
	require.Len(t, listPaymentsResp.Payments, numPayments)
}

func payNode(invoiceCtx, paymentCtx context.Context, t *harnessTest,
	from routerrpc.RouterClient, to lnrpc.LightningClient, amt int64,
	memo string) {

	invoice, err := to.AddInvoice(invoiceCtx, &lnrpc.Invoice{
		Value: amt,
		Memo:  memo,
	})
	require.NoError(t.t, err)

	sendReq := &routerrpc.SendPaymentRequest{
		PaymentRequest: invoice.PaymentRequest,
		TimeoutSeconds: 2,
		FeeLimitMsat:   1000,
	}
	stream, err := from.SendPaymentV2(paymentCtx, sendReq)
	require.NoError(t.t, err)

	result, err := getPaymentResult(stream)
	require.NoError(t.t, err)
	require.Equal(t.t, result.Status, lnrpc.Payment_SUCCEEDED)
}

func getPaymentResult(stream routerrpc.Router_SendPaymentV2Client) (
	*lnrpc.Payment, error) {

	for {
		payment, err := stream.Recv()
		if err != nil {
			return nil, err
		}

		if payment.Status != lnrpc.Payment_IN_FLIGHT {
			return payment, nil
		}
	}
}
