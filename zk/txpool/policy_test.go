package txpool

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/c2h5oh/datasize"
	mdbx2 "github.com/erigontech/mdbx-go/mdbx"
	"github.com/ledgerwatch/erigon-lib/common"
	"github.com/ledgerwatch/erigon-lib/kv"
	"github.com/ledgerwatch/erigon-lib/kv/mdbx"
	"github.com/ledgerwatch/log/v3"
	"github.com/stretchr/testify/require"
)

// newTestState creates new instance of state used by tests.
func newTestACLDB(tb testing.TB, dir string) kv.RwDB {
	tb.Helper()

	if dir == "" {
		dir = tb.TempDir()
	}

	state, err := OpenACLDB(context.Background(), dir)
	if err != nil {
		tb.Fatal(err)
	}

	tb.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			tb.Fatal(err)
		}
	})

	return state
}

func newTestTxPoolDB(tb testing.TB, dir string) kv.RwDB {
	tb.Helper()

	if dir == "" {
		dir = fmt.Sprintf("/tmp/txpool-db-temp_%v", time.Now().UTC().Format(time.RFC3339Nano))
	}

	err := os.Mkdir(dir, 0775)
	if err != nil {
		tb.Fatal(err)
	}

	txPoolDB, err := mdbx.NewMDBX(log.New()).Label(kv.TxPoolDB).Path(dir).
		WithTableCfg(func(defaultBuckets kv.TableCfg) kv.TableCfg { return kv.TxpoolTablesCfg }).
		Flags(func(f uint) uint { return f ^ mdbx2.Durable | mdbx2.SafeNoSync }).
		GrowthStep(16 * datasize.MB).
		SyncPeriod(30 * time.Second).
		Open(context.Background())
	if err != nil {
		tb.Fatal(err)
	}

	tb.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			tb.Fatal(err)
		}
	})

	return txPoolDB
}

func policyTransactionSliceEqual(a, b []PolicyTransaction) bool {
	// Check if lengths are different
	if len(a) != len(b) {
		return false
	}

	// Check each element, excluding timeTx
	for i := range a {
		if a[i].aclType != b[i].aclType {
			return false
		}
		if ACLTypeBinary(a[i].operation) != ACLTypeBinary(b[i].operation) {
			return false
		}
		if a[i].addr != b[i].addr {
			return false
		}
		if a[i].policy != b[i].policy {
			return false
		}
	}

	return true
}

func containsSubstring(slice []string, substring string) bool {
	for _, str := range slice {
		if strings.Contains(str, substring) {
			return true
		}
	}
	return false
}

func TestCheckDBsCreation(t *testing.T) {
	t.Parallel()

	path := fmt.Sprintf("/tmp/db-test-%v", time.Now().UTC().Format(time.RFC3339Nano))

	txPoolDB := newTestTxPoolDB(t, path)
	aclsDB := newTestACLDB(t, path)

	// Check if the dbs are created
	require.NotNil(t, txPoolDB)
	require.NotNil(t, aclsDB)
}

func TestSetMode(t *testing.T) {
	t.Parallel()

	db := newTestACLDB(t, "")
	ctx := context.Background()

	t.Run("SetMode - Valid Mode", func(t *testing.T) {
		t.Parallel()

		mode := AllowlistMode

		err := SetMode(ctx, db, mode)
		require.NoError(t, err)

		// Check if the mode is set correctly
		modeInDB, err := GetMode(ctx, db)
		require.NoError(t, err)
		require.Equal(t, string(mode), string(modeInDB))
	})

	t.Run("SetMode - Invalid Mode", func(t *testing.T) {
		t.Parallel()

		mode := "invalid_mode"

		err := SetMode(ctx, db, mode)
		require.ErrorIs(t, err, errInvalidMode)
	})
}

func TestRemovePolicy(t *testing.T) {
	t.Parallel()

	db := newTestACLDB(t, "")
	ctx := context.Background()

	SetMode(ctx, db, BlocklistMode)

	t.Run("RemovePolicy - Policy Exists", func(t *testing.T) {
		t.Parallel()

		// Create a test address and policy
		addr := common.HexToAddress("0x1234567890abcdef")
		policy := SendTx

		// Add the policy to the ACL
		require.NoError(t, AddPolicy(ctx, db, "blocklist", addr, policy))

		// Remove the policy from the ACL
		err := RemovePolicy(ctx, db, "blocklist", addr, policy)
		require.NoError(t, err)

		// Check if the policy is removed from the ACL
		hasPolicy, err := DoesAccountHavePolicy(ctx, db, addr, policy)
		require.NoError(t, err)
		require.False(t, hasPolicy)
	})

	t.Run("RemovePolicy - Policy Does Not Exist", func(t *testing.T) {
		t.Parallel()

		// Create a test address and policy
		addr := common.HexToAddress("0x1234567890abcdef")
		policy := SendTx

		// Add some different policy to the ACL
		require.NoError(t, AddPolicy(ctx, db, "blocklist", addr, Deploy))

		// Remove the policy from the ACL
		err := RemovePolicy(ctx, db, "blocklist", addr, policy)
		require.NoError(t, err)

		// Check if the policy is still not present in the ACL
		hasPolicy, err := DoesAccountHavePolicy(ctx, db, addr, policy)
		require.NoError(t, err)
		require.False(t, hasPolicy)
	})

	t.Run("RemovePolicy - Address Not Found", func(t *testing.T) {
		t.Parallel()

		// Create a test address and policy
		addr := common.HexToAddress("0x1234567890abcdef")
		policy := SendTx

		// Remove the policy from the ACL
		err := RemovePolicy(ctx, db, "blocklist", addr, policy)
		require.NoError(t, err)

		// Check if the policy is still not present in the ACL
		hasPolicy, err := DoesAccountHavePolicy(ctx, db, addr, policy)
		require.NoError(t, err)
		require.False(t, hasPolicy)
	})

	t.Run("RemovePolicy - Unsupported acl type", func(t *testing.T) {
		t.Parallel()

		// Create a test address and policy
		addr := common.HexToAddress("0x1234567890abcdef")
		policy := SendTx

		err := RemovePolicy(ctx, db, "unknown_acl_type", addr, policy)
		require.ErrorIs(t, err, errUnsupportedACLType)
	})
}

func TestPolicyMapping(t *testing.T) {
	// All policies
	var policiesAll []byte
	var pListAll []Policy
	policiesAll = append(policiesAll, SendTx.ToByte())
	policiesAll = append(policiesAll, Deploy.ToByte())
	pListAll = append(pListAll, SendTx)
	pListAll = append(pListAll, Deploy)

	// Only sendTx policy
	var policiesSendTx []byte
	var pListSendTx []Policy
	policiesSendTx = append(policiesSendTx, SendTx.ToByte())
	pListSendTx = append(pListSendTx, SendTx)

	// Only deploy policy
	var policiesDeploy []byte
	var pListDeploy []Policy
	policiesDeploy = append(policiesDeploy, Deploy.ToByte())
	pListDeploy = append(pListDeploy, Deploy)

	// No policy
	var policiesNone []byte
	var pListNone []Policy

	// Expected outcomes - these are stored in []string, because reading policies doesn't guarantee order, and the returned values may be in arbitrary order.
	// Therefore a []string is used to check if the returned values are within the expected combinations stored within the string slice.
	var expectedAll []string
	var expectedSendTx []string
	var expectedDeploy []string
	var expectedNone []string

	expectedAll = append(expectedAll, "\tsendTx: true\n\tdeploy: true")
	expectedAll = append(expectedAll, "\tdeploy: true\n\tsendTx: true")
	expectedSendTx = append(expectedSendTx, "\tsendTx: true")
	expectedDeploy = append(expectedDeploy, "\tdeploy: true")
	expectedNone = append(expectedNone, "")

	var tests = []struct {
		policies []byte
		pList    []Policy
		want     []string
	}{
		{policiesAll, pListAll, expectedAll},
		{policiesSendTx, pListSendTx, expectedSendTx},
		{policiesDeploy, pListDeploy, expectedDeploy},
		{policiesNone, pListNone, expectedNone},
	}
	for _, tt := range tests {
		t.Run("PolicyMapping", func(t *testing.T) {
			ans := policyMapping(tt.policies, tt.pList)
			if !containsSubstring(tt.want, ans) {
				t.Errorf("got %v, want %v", ans, tt.want)
			}
		})
	}
}

func TestAddPolicy(t *testing.T) {
	t.Parallel()

	db := newTestACLDB(t, "")
	ctx := context.Background()

	SetMode(ctx, db, BlocklistMode)

	t.Run("AddPolicy - Policy Does Not Exist", func(t *testing.T) {
		t.Parallel()

		// Create a test address and policy
		addr := common.HexToAddress("0x1234567890abcdef")
		policy := SendTx

		err := AddPolicy(ctx, db, "blocklist", addr, policy)
		require.NoError(t, err)

		// Check if the policy exists in the ACL
		hasPolicy, err := DoesAccountHavePolicy(ctx, db, addr, policy)
		require.NoError(t, err)
		require.True(t, hasPolicy)
	})

	t.Run("AddPolicy - Policy Already Exists", func(t *testing.T) {
		t.Parallel()

		// Create a test address and policy
		addr := common.HexToAddress("0x1234567890abcdef")
		policy := SendTx

		// Add the policy to the ACL
		require.NoError(t, AddPolicy(ctx, db, "blocklist", addr, policy))

		// Add the policy again
		err := AddPolicy(ctx, db, "blocklist", addr, policy)
		require.NoError(t, err)

		// Check if the policy still exists in the ACL
		hasPolicy, err := DoesAccountHavePolicy(ctx, db, addr, policy)
		require.NoError(t, err)
		require.True(t, hasPolicy)
	})

	t.Run("AddPolicy - Unsupported Policy", func(t *testing.T) {
		t.Parallel()

		// Create a test address and policy
		addr := common.HexToAddress("0x1234567890abcdef")
		policy := Policy(33) // Assume Policy(33) is not supported

		err := AddPolicy(ctx, db, "blocklist", addr, policy)
		require.ErrorIs(t, err, errUnknownPolicy)
	})

	t.Run("AddPolicy - Unsupported acl type", func(t *testing.T) {
		t.Parallel()

		// Create a test address and policy
		addr := common.HexToAddress("0x1234567890abcdef")
		policy := SendTx

		err := AddPolicy(ctx, db, "unknown_acl_type", addr, policy)
		require.ErrorIs(t, err, errUnsupportedACLType)
	})
}

func TestUpdatePolicies(t *testing.T) {
	t.Parallel()

	db := newTestACLDB(t, "")
	ctx := context.Background()

	SetMode(ctx, db, BlocklistMode)

	t.Run("UpdatePolicies - Add Policies", func(t *testing.T) {
		t.Parallel()

		// Create test addresses and policies
		addr1 := common.HexToAddress("0x1234567890abcdef")
		addr2 := common.HexToAddress("0xabcdef1234567890")
		policies := [][]Policy{
			{SendTx, Deploy},
			{SendTx},
		}

		err := UpdatePolicies(ctx, db, "blocklist", []common.Address{addr1, addr2}, policies)
		require.NoError(t, err)

		// Check if the policies are added correctly
		hasPolicy, err := DoesAccountHavePolicy(ctx, db, addr1, SendTx)
		require.NoError(t, err)
		require.True(t, hasPolicy)

		hasPolicy, err = DoesAccountHavePolicy(ctx, db, addr1, Deploy)
		require.NoError(t, err)
		require.True(t, hasPolicy)

		hasPolicy, err = DoesAccountHavePolicy(ctx, db, addr2, SendTx)
		require.NoError(t, err)
		require.True(t, hasPolicy)
	})

	t.Run("UpdatePolicies - Remove Policies", func(t *testing.T) {
		t.Parallel()

		// Create test addresses and policies
		addr1 := common.HexToAddress("0x1234567890abcdea")
		addr2 := common.HexToAddress("0xabcdef1234567891")
		policiesOld := [][]Policy{
			{SendTx, Deploy},
			{SendTx},
		}

		err := UpdatePolicies(ctx, db, "blocklist", []common.Address{addr1, addr2}, policiesOld)
		require.NoError(t, err)

		// Check if the policies are added correctly
		hasPolicy, err := DoesAccountHavePolicy(ctx, db, addr1, SendTx)
		require.NoError(t, err)
		require.True(t, hasPolicy)

		hasPolicy, err = DoesAccountHavePolicy(ctx, db, addr1, Deploy)
		require.NoError(t, err)
		require.True(t, hasPolicy)

		hasPolicy, err = DoesAccountHavePolicy(ctx, db, addr2, SendTx)
		require.NoError(t, err)
		require.True(t, hasPolicy)

		policies := [][]Policy{
			{},
			{SendTx},
		}

		err = UpdatePolicies(ctx, db, "blocklist", []common.Address{addr1, addr2}, policies)
		require.NoError(t, err)

		// Check if the policies are removed correctly
		hasPolicy, err = DoesAccountHavePolicy(ctx, db, addr1, SendTx)
		require.NoError(t, err)
		require.False(t, hasPolicy)

		hasPolicy, err = DoesAccountHavePolicy(ctx, db, addr1, Deploy)
		require.NoError(t, err)
		require.False(t, hasPolicy)

		hasPolicy, err = DoesAccountHavePolicy(ctx, db, addr2, SendTx)
		require.NoError(t, err)
		require.True(t, hasPolicy)
	})

	t.Run("UpdatePolicies - Empty Policies", func(t *testing.T) {
		t.Parallel()

		// Create test addresses and policies
		addr1 := common.HexToAddress("0x1234567890abcded")
		addr2 := common.HexToAddress("0xabcdef1234567893")

		// first add these policies
		policiesOld := [][]Policy{
			{SendTx, Deploy},
			{SendTx},
		}

		err := UpdatePolicies(ctx, db, "blocklist", []common.Address{addr1, addr2}, policiesOld)
		require.NoError(t, err)

		// Check if the policies are added correctly
		hasPolicy, err := DoesAccountHavePolicy(ctx, db, addr1, SendTx)
		require.NoError(t, err)
		require.True(t, hasPolicy)

		hasPolicy, err = DoesAccountHavePolicy(ctx, db, addr1, Deploy)
		require.NoError(t, err)
		require.True(t, hasPolicy)

		hasPolicy, err = DoesAccountHavePolicy(ctx, db, addr2, SendTx)
		require.NoError(t, err)
		require.True(t, hasPolicy)

		// then remove policies
		policies := [][]Policy{
			{},
			{},
		}

		err = UpdatePolicies(ctx, db, "blocklist", []common.Address{addr1, addr2}, policies)
		require.NoError(t, err)

		// Check if the policies are removed correctly
		hasPolicy, err = DoesAccountHavePolicy(ctx, db, addr1, SendTx)
		require.NoError(t, err)
		require.False(t, hasPolicy)

		hasPolicy, err = DoesAccountHavePolicy(ctx, db, addr1, Deploy)
		require.NoError(t, err)
		require.False(t, hasPolicy)

		hasPolicy, err = DoesAccountHavePolicy(ctx, db, addr2, SendTx)
		require.NoError(t, err)
		require.False(t, hasPolicy)
	})

	t.Run("UpdatePolicies - Unsupported acl type", func(t *testing.T) {
		t.Parallel()

		// Create test addresses and policies
		addr1 := common.HexToAddress("0x1234567890abcdef")
		addr2 := common.HexToAddress("0xabcdef1234567890")
		policies := [][]Policy{
			{SendTx, Deploy},
			{SendTx},
		}

		err := UpdatePolicies(ctx, db, "unknown_acl_type", []common.Address{addr1, addr2}, policies)
		require.ErrorIs(t, err, errUnsupportedACLType)
	})
}

func TestLastPolicyTransactions(t *testing.T) {
	db := newTestACLDB(t, "")
	ctx := context.Background()

	SetMode(ctx, db, BlocklistMode)

	// Create a test address and policy
	addrInit := common.HexToAddress("0x0000000000000000")
	policyInit := SendTx

	addrOne := common.HexToAddress("0x1234567890abcdef")
	policyOne := SendTx

	addrTwo := common.HexToAddress("0xabcdef1234567890")
	policyTwo := SendTx

	// Add the policy to the ACL
	require.NoError(t, AddPolicy(ctx, db, "blocklist", addrInit, policyInit))
	require.NoError(t, AddPolicy(ctx, db, "blocklist", addrOne, policyOne))
	require.NoError(t, AddPolicy(ctx, db, "blocklist", addrTwo, policyTwo))

	// Create expected policyTransaction output and append to []PolicyTransaction
	policyTransactionInit := PolicyTransaction{addr: common.HexToAddress("0x0000000000000000"), aclType: ResolveACLTypeToBinary("blocklist"), policy: Policy(SendTx.ToByte()), operation: Operation(ModeChange.ToByte())}
	policyTransactionOne := PolicyTransaction{addr: common.HexToAddress("0x1234567890abcdef"), aclType: ResolveACLTypeToBinary("blocklist"), policy: Policy(SendTx.ToByte()), operation: Operation(Add.ToByte())}
	policyTransactionTwo := PolicyTransaction{addr: common.HexToAddress("0xabcdef1234567890"), aclType: ResolveACLTypeToBinary("blocklist"), policy: Policy(SendTx.ToByte()), operation: Operation(Add.ToByte())}

	// LastPolicyTransactions seems to append in reverse order than this test function. So the order of elements is also reversed
	// No element in PolicyTransaction slice
	var policyTransactionSliceNone []PolicyTransaction

	// Single element in PolicyTransaction slice, always starting with policyTransactionInit
	var policyTransactionSliceSingle []PolicyTransaction
	policyTransactionSliceSingle = append(policyTransactionSliceSingle, policyTransactionInit)

	// Two elements in PolicyTransaction slice, always starting with policyTransactionInit
	var policyTransactionSliceDouble []PolicyTransaction
	policyTransactionSliceDouble = append(policyTransactionSliceDouble, policyTransactionInit)
	policyTransactionSliceDouble = append(policyTransactionSliceDouble, policyTransactionTwo)

	// Three elements in PolicyTransaction slice, always starting with policyTransactionInit
	var policyTransactionSliceTriple []PolicyTransaction
	policyTransactionSliceTriple = append(policyTransactionSliceTriple, policyTransactionInit)
	policyTransactionSliceTriple = append(policyTransactionSliceTriple, policyTransactionTwo)
	policyTransactionSliceTriple = append(policyTransactionSliceTriple, policyTransactionOne)

	// Table driven test
	var tests = []struct {
		count int
		want  []PolicyTransaction
	}{
		{0, policyTransactionSliceNone},
		{1, policyTransactionSliceSingle},
		{2, policyTransactionSliceDouble},
		{3, policyTransactionSliceTriple},
	}
	for _, tt := range tests {
		t.Run("LastPolicyTransactions", func(t *testing.T) {
			ans, err := LastPolicyTransactions(ctx, db, tt.count)
			if err != nil {
				t.Errorf("LastPolicyTransactions did not execute successfully: %v", err)
			}
			if !policyTransactionSliceEqual(ans, tt.want) {
				t.Errorf("got %v, want %v", ans, tt.want)
			}
		})
	}
}

func TestIsActionAllowed(t *testing.T) {
	db := newTestACLDB(t, "")
	ctx := context.Background()

	txPool := &TxPool{policyValidator: NewPolicyValidator(db)}

	t.Run("isActionAllowed - BlocklistMode - Policy Exists", func(t *testing.T) {
		SetMode(ctx, db, BlocklistMode)

		// Create a test address and policy
		addr := common.HexToAddress("0x1234567890abcdef")
		policy := SendTx

		// Add the policy to the ACL
		require.NoError(t, AddPolicy(ctx, db, "blocklist", addr, policy))

		// Check if the action is allowed
		allowed, err := txPool.policyValidator.IsActionAllowed(ctx, addr, policy.ToByte())
		require.NoError(t, err)
		require.False(t, allowed) // In blocklist mode, having the policy means the action is not allowed
	})

	t.Run("isActionAllowed - BlocklistMode - Policy Does Not Exist", func(t *testing.T) {
		SetMode(ctx, db, BlocklistMode)

		// Create a test address and policy
		addr := common.HexToAddress("0x1234567890abcdef")
		policy := Deploy

		// Check if the action is allowed
		allowed, err := txPool.policyValidator.IsActionAllowed(ctx, addr, policy.ToByte())
		require.NoError(t, err)
		require.True(t, allowed) // In blocklist mode, not having the policy means the action is allowed
	})

	t.Run("isActionAllowed - AllowlistMode - Policy Exists", func(t *testing.T) {
		SetMode(ctx, db, AllowlistMode)

		// Create a test address and policy
		addr := common.HexToAddress("0x1234567890abcdef")
		policy := SendTx

		// Add the policy to the ACL
		require.NoError(t, AddPolicy(ctx, db, "allowlist", addr, policy))

		// Check if the action is allowed
		allowed, err := txPool.policyValidator.IsActionAllowed(ctx, addr, policy.ToByte())
		require.NoError(t, err)
		require.True(t, allowed) // In allowlist mode, having the policy means the action is allowed
	})

	t.Run("isActionAllowed - AllowlistMode - Policy Does Not Exist", func(t *testing.T) {
		SetMode(ctx, db, AllowlistMode)

		// Create a test address and policy
		addr := common.HexToAddress("0x1234567890abcdef")
		policy := Deploy

		// Check if the action is allowed
		allowed, err := txPool.policyValidator.IsActionAllowed(ctx, addr, policy.ToByte())
		require.NoError(t, err)
		require.False(t, allowed) // In allowlist mode, not having the policy means the action is not allowed
	})

	t.Run("isActionAllowed - DisabledMode", func(t *testing.T) {
		SetMode(ctx, db, DisabledMode)

		// Create a test address and policy
		addr := common.HexToAddress("0x1234567890abcdef")
		policy := SendTx

		// Check if the action is allowed
		allowed, err := txPool.policyValidator.IsActionAllowed(ctx, addr, policy.ToByte())
		require.NoError(t, err)
		require.True(t, allowed) // In disabled mode, all actions are allowed
	})
}

func TestListContentAtACL(t *testing.T) {
	db := newTestACLDB(t, "")
	ctx := context.Background()

	// Populate different tables in ACL
	// Create a test address and policy for allowlist table
	addrAllowlist := common.HexToAddress("0x1234567890abcdef")
	policyAllowlist := SendTx

	err := AddPolicy(ctx, db, "allowlist", addrAllowlist, policyAllowlist)
	require.NoError(t, err)

	// Create a test address and policy for blocklist table
	addrBlocklist := common.HexToAddress("0x1234567890abcdef")
	policyBlocklist := SendTx

	err = AddPolicy(ctx, db, "blocklist", addrBlocklist, policyBlocklist)
	require.NoError(t, err)

	var tests = []struct {
		wantAllowlist string
		wantBlockList string
	}{
		{"\nAllowlist\nKey: 0000000000000000000000001234567890abcdef, Value: {\n\tdeploy: false\n\tsendTx: true\n}\n", "\nBlocklist\nKey: 0000000000000000000000001234567890abcdef, Value: {\n\tsendTx: true\n\tdeploy: false\n}\n"},
	}
	// ListContentAtACL will return []string in the following order:
	// [buffer.String(), bufferConfig.String(), bufferBlockList.String(), bufferAllowlist.String()]
	ans, err := ListContentAtACL(ctx, db)
	for _, tt := range tests {
		t.Run("ListContentAtACL", func(t *testing.T) {
			switch {
			case err != nil:
				t.Errorf("ListContentAtACL did not execute successfully: %v", err)
			case !strings.Contains(ans[3], "\nAllowlist\nKey: 0000000000000000000000001234567890abcdef"):
				t.Errorf("got %v, want %v", ans, tt.wantAllowlist)
			case !strings.Contains(ans[3], "sendTx: true"):
				t.Errorf("got %v, want %v", ans, tt.wantAllowlist)
			case !strings.Contains(ans[3], "deploy: false"):
				t.Errorf("got %v, want %v", ans, tt.wantAllowlist)
			case !strings.Contains(ans[2], "\nBlocklist\nKey: 0000000000000000000000001234567890abcdef"):
				t.Errorf("got %v, want %v", ans, tt.wantBlockList)
			case !strings.Contains(ans[2], "sendTx: true"):
				t.Errorf("got %v, want %v", ans, tt.wantBlockList)
			case !strings.Contains(ans[2], "deploy: false"):
				t.Errorf("got %v, want %v", ans, tt.wantBlockList)
			}
		})
	}
}
