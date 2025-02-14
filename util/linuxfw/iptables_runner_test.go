// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

//go:build linux

package linuxfw

import (
	"errors"
	"net/netip"
	"strings"
	"testing"

	"tailscale.com/net/tsaddr"
)

var errExec = errors.New("execution failed")

type fakeIPTables struct {
	t *testing.T
	n map[string][]string
}

type fakeRule struct {
	table, chain string
	args         []string
}

func newIPTables(t *testing.T) *fakeIPTables {
	return &fakeIPTables{
		t: t,
		n: map[string][]string{
			"filter/INPUT":    nil,
			"filter/OUTPUT":   nil,
			"filter/FORWARD":  nil,
			"nat/PREROUTING":  nil,
			"nat/OUTPUT":      nil,
			"nat/POSTROUTING": nil,
		},
	}
}

func (n *fakeIPTables) Insert(table, chain string, pos int, args ...string) error {
	k := table + "/" + chain
	if rules, ok := n.n[k]; ok {
		if pos > len(rules)+1 {
			n.t.Errorf("bad position %d in %s", pos, k)
			return errExec
		}
		rules = append(rules, "")
		copy(rules[pos:], rules[pos-1:])
		rules[pos-1] = strings.Join(args, " ")
		n.n[k] = rules
	} else {
		n.t.Errorf("unknown table/chain %s", k)
		return errExec
	}
	return nil
}

func (n *fakeIPTables) Append(table, chain string, args ...string) error {
	k := table + "/" + chain
	return n.Insert(table, chain, len(n.n[k])+1, args...)
}

func (n *fakeIPTables) Exists(table, chain string, args ...string) (bool, error) {
	k := table + "/" + chain
	if rules, ok := n.n[k]; ok {
		for _, rule := range rules {
			if rule == strings.Join(args, " ") {
				return true, nil
			}
		}
		return false, nil
	} else {
		n.t.Logf("unknown table/chain %s", k)
		return false, errExec
	}
}

func hasChain(n *fakeIPTables, table, chain string) bool {
	k := table + "/" + chain
	if _, ok := n.n[k]; ok {
		return true
	} else {
		return false
	}
}

func (n *fakeIPTables) Delete(table, chain string, args ...string) error {
	k := table + "/" + chain
	if rules, ok := n.n[k]; ok {
		for i, rule := range rules {
			if rule == strings.Join(args, " ") {
				rules = append(rules[:i], rules[i+1:]...)
				n.n[k] = rules
				return nil
			}
		}
		n.t.Errorf("delete of unknown rule %q from %s", strings.Join(args, " "), k)
		return errExec
	} else {
		n.t.Errorf("unknown table/chain %s", k)
		return errExec
	}
}

func (n *fakeIPTables) ClearChain(table, chain string) error {
	k := table + "/" + chain
	if _, ok := n.n[k]; ok {
		n.n[k] = nil
		return nil
	} else {
		n.t.Logf("note: ClearChain: unknown table/chain %s", k)
		return errors.New("exitcode:1")
	}
}

func (n *fakeIPTables) NewChain(table, chain string) error {
	k := table + "/" + chain
	if _, ok := n.n[k]; ok {
		n.t.Errorf("table/chain %s already exists", k)
		return errExec
	}
	n.n[k] = nil
	return nil
}

func (n *fakeIPTables) DeleteChain(table, chain string) error {
	k := table + "/" + chain
	if rules, ok := n.n[k]; ok {
		if len(rules) != 0 {
			n.t.Errorf("%s is not empty", k)
			return errExec
		}
		delete(n.n, k)
		return nil
	} else {
		n.t.Errorf("%s does not exist", k)
		return errExec
	}
}

func newFakeIPTablesRunner(t *testing.T) *iptablesRunner {
	ipt4 := newIPTables(t)
	ipt6 := newIPTables(t)

	iptr := &iptablesRunner{ipt4, ipt6, true, true}
	return iptr
}

func TestAddAndDeleteChains(t *testing.T) {
	iptr := newFakeIPTablesRunner(t)
	err := iptr.AddChains()
	if err != nil {
		t.Fatal(err)
	}

	// Check that the chains were created.
	tsChains := []struct{ table, chain string }{ // table/chain
		{"filter", "ts-input"},
		{"filter", "ts-forward"},
		{"nat", "ts-postrouting"},
	}

	for _, proto := range []iptablesInterface{iptr.ipt4, iptr.ipt6} {
		for _, tc := range tsChains {
			// Exists returns error if the chain doesn't exist.
			if _, err := proto.Exists(tc.table, tc.chain); err != nil {
				t.Errorf("chain %s/%s doesn't exist", tc.table, tc.chain)
			}
		}
	}

	err = iptr.DelChains()
	if err != nil {
		t.Fatal(err)
	}

	// Check that the chains were deleted.
	for _, proto := range []iptablesInterface{iptr.ipt4, iptr.ipt6} {
		for _, tc := range tsChains {
			if _, err = proto.Exists(tc.table, tc.chain); err == nil {
				t.Errorf("chain %s/%s still exists", tc.table, tc.chain)
			}
		}
	}

}

func TestAddAndDeleteHooks(t *testing.T) {
	iptr := newFakeIPTablesRunner(t)
	// don't need to test what happens if the chains don't exist, because
	// this is handled by fake iptables, in realife iptables would return error.
	if err := iptr.AddChains(); err != nil {
		t.Fatal(err)
	}
	defer iptr.DelChains()

	if err := iptr.AddHooks(); err != nil {
		t.Fatal(err)
	}

	// Check that the rules were created.
	tsRules := []fakeRule{ // table/chain/rule
		{"filter", "INPUT", []string{"-j", "ts-input"}},
		{"filter", "FORWARD", []string{"-j", "ts-forward"}},
		{"nat", "POSTROUTING", []string{"-j", "ts-postrouting"}},
	}

	for _, proto := range []iptablesInterface{iptr.ipt4, iptr.ipt6} {
		for _, tr := range tsRules {
			if exists, err := proto.Exists(tr.table, tr.chain, tr.args...); err != nil {
				t.Fatal(err)
			} else if !exists {
				t.Errorf("rule %s/%s/%s doesn't exist", tr.table, tr.chain, strings.Join(tr.args, " "))
			}
			// check if the rule is at front of the chain
			if proto.(*fakeIPTables).n[tr.table+"/"+tr.chain][0] != strings.Join(tr.args, " ") {
				t.Errorf("v4 rule %s/%s/%s is not at the top", tr.table, tr.chain, strings.Join(tr.args, " "))
			}
		}
	}

	if err := iptr.DelHooks(t.Logf); err != nil {
		t.Fatal(err)
	}

	// Check that the rules were deleted.
	for _, proto := range []iptablesInterface{iptr.ipt4, iptr.ipt6} {
		for _, tr := range tsRules {
			if exists, err := proto.Exists(tr.table, tr.chain, tr.args...); err != nil {
				t.Fatal(err)
			} else if exists {
				t.Errorf("rule %s/%s/%s still exists", tr.table, tr.chain, strings.Join(tr.args, " "))
			}
		}
	}

	if err := iptr.AddHooks(); err != nil {
		t.Fatal(err)
	}
}

func TestAddAndDeleteBase(t *testing.T) {
	iptr := newFakeIPTablesRunner(t)
	tunname := "tun0"
	if err := iptr.AddChains(); err != nil {
		t.Fatal(err)
	}

	if err := iptr.AddBase(tunname); err != nil {
		t.Fatal(err)
	}

	// Check that the rules were created.
	tsRulesV4 := []fakeRule{ // table/chain/rule
		{"filter", "ts-input", []string{"!", "-i", tunname, "-s", tsaddr.ChromeOSVMRange().String(), "-j", "RETURN"}},
		{"filter", "ts-input", []string{"!", "-i", tunname, "-s", tsaddr.CGNATRange().String(), "-j", "DROP"}},
		{"filter", "ts-forward", []string{"-o", tunname, "-s", tsaddr.CGNATRange().String(), "-j", "DROP"}},
	}

	tsRulesCommon := []fakeRule{ // table/chain/rule
		{"filter", "ts-input", []string{"-i", tunname, "-j", "ACCEPT"}},
		{"filter", "ts-forward", []string{"-i", tunname, "-j", "MARK", "--set-mark", TailscaleSubnetRouteMark + "/" + TailscaleFwmarkMask}},
		{"filter", "ts-forward", []string{"-m", "mark", "--mark", TailscaleSubnetRouteMark + "/" + TailscaleFwmarkMask, "-j", "ACCEPT"}},
		{"filter", "ts-forward", []string{"-o", tunname, "-j", "ACCEPT"}},
	}

	// check that the rules were created for ipt4
	for _, tr := range append(tsRulesV4, tsRulesCommon...) {
		if exists, err := iptr.ipt4.Exists(tr.table, tr.chain, tr.args...); err != nil {
			t.Fatal(err)
		} else if !exists {
			t.Errorf("rule %s/%s/%s doesn't exist", tr.table, tr.chain, strings.Join(tr.args, " "))
		}
	}

	// check that the rules were created for ipt6
	for _, tr := range tsRulesCommon {
		if exists, err := iptr.ipt6.Exists(tr.table, tr.chain, tr.args...); err != nil {
			t.Fatal(err)
		} else if !exists {
			t.Errorf("rule %s/%s/%s doesn't exist", tr.table, tr.chain, strings.Join(tr.args, " "))
		}
	}

	if err := iptr.DelBase(); err != nil {
		t.Fatal(err)
	}

	// Check that the rules were deleted.
	for _, proto := range []iptablesInterface{iptr.ipt4, iptr.ipt6} {
		for _, tr := range append(tsRulesV4, tsRulesCommon...) {
			if exists, err := proto.Exists(tr.table, tr.chain, tr.args...); err != nil {
				t.Fatal(err)
			} else if exists {
				t.Errorf("rule %s/%s/%s still exists", tr.table, tr.chain, strings.Join(tr.args, " "))
			}
		}
	}

	if err := iptr.DelChains(); err != nil {
		t.Fatal(err)
	}
}

func TestAddAndDelLoopbackRule(t *testing.T) {
	iptr := newFakeIPTablesRunner(t)
	// We don't need to test for malformed addresses, AddLoopbackRule
	// takes in a netip.Addr, which is already valid.
	fakeAddrV4 := netip.MustParseAddr("192.168.0.2")
	fakeAddrV6 := netip.MustParseAddr("2001:db8::2")

	if err := iptr.AddChains(); err != nil {
		t.Fatal(err)
	}
	if err := iptr.AddLoopbackRule(fakeAddrV4); err != nil {
		t.Fatal(err)
	}
	if err := iptr.AddLoopbackRule(fakeAddrV6); err != nil {
		t.Fatal(err)
	}

	// Check that the rules were created.
	tsRulesV4 := fakeRule{ // table/chain/rule
		"filter", "ts-input", []string{"-i", "lo", "-s", fakeAddrV4.String(), "-j", "ACCEPT"}}

	tsRulesV6 := fakeRule{ // table/chain/rule
		"filter", "ts-input", []string{"-i", "lo", "-s", fakeAddrV6.String(), "-j", "ACCEPT"}}

	// check that the rules were created for ipt4 and ipt6
	if exist, err := iptr.ipt4.Exists(tsRulesV4.table, tsRulesV4.chain, tsRulesV4.args...); err != nil {
		t.Fatal(err)
	} else if !exist {
		t.Errorf("rule %s/%s/%s doesn't exist", tsRulesV4.table, tsRulesV4.chain, strings.Join(tsRulesV4.args, " "))
	}
	if exist, err := iptr.ipt6.Exists(tsRulesV6.table, tsRulesV6.chain, tsRulesV6.args...); err != nil {
		t.Fatal(err)
	} else if !exist {
		t.Errorf("rule %s/%s/%s doesn't exist", tsRulesV6.table, tsRulesV6.chain, strings.Join(tsRulesV6.args, " "))
	}

	// check that the rule is at the top
	chain := "filter/ts-input"
	if iptr.ipt4.(*fakeIPTables).n[chain][0] != strings.Join(tsRulesV4.args, " ") {
		t.Errorf("v4 rule %s/%s/%s is not at the top", tsRulesV4.table, tsRulesV4.chain, strings.Join(tsRulesV4.args, " "))
	}
	if iptr.ipt6.(*fakeIPTables).n[chain][0] != strings.Join(tsRulesV6.args, " ") {
		t.Errorf("v6 rule %s/%s/%s is not at the top", tsRulesV6.table, tsRulesV6.chain, strings.Join(tsRulesV6.args, " "))
	}

	// delete the rules
	if err := iptr.DelLoopbackRule(fakeAddrV4); err != nil {
		t.Fatal(err)
	}
	if err := iptr.DelLoopbackRule(fakeAddrV6); err != nil {
		t.Fatal(err)
	}

	// Check that the rules were deleted.
	if exist, err := iptr.ipt4.Exists(tsRulesV4.table, tsRulesV4.chain, tsRulesV4.args...); err != nil {
		t.Fatal(err)
	} else if exist {
		t.Errorf("rule %s/%s/%s still exists", tsRulesV4.table, tsRulesV4.chain, strings.Join(tsRulesV4.args, " "))
	}

	if exist, err := iptr.ipt6.Exists(tsRulesV6.table, tsRulesV6.chain, tsRulesV6.args...); err != nil {
		t.Fatal(err)
	} else if exist {
		t.Errorf("rule %s/%s/%s still exists", tsRulesV6.table, tsRulesV6.chain, strings.Join(tsRulesV6.args, " "))
	}

	if err := iptr.DelChains(); err != nil {
		t.Fatal(err)
	}
}

func TestAddAndDelSNATRule(t *testing.T) {
	iptr := newFakeIPTablesRunner(t)

	if err := iptr.AddChains(); err != nil {
		t.Fatal(err)
	}

	rule := fakeRule{ // table/chain/rule
		"nat", "ts-postrouting", []string{"-m", "mark", "--mark", TailscaleSubnetRouteMark + "/" + TailscaleFwmarkMask, "-j", "MASQUERADE"},
	}

	// Add SNAT rule
	if err := iptr.AddSNATRule(); err != nil {
		t.Fatal(err)
	}

	// Check that the rule was created for ipt4 and ipt6
	for _, proto := range []iptablesInterface{iptr.ipt4, iptr.ipt6} {
		if exist, err := proto.Exists(rule.table, rule.chain, rule.args...); err != nil {
			t.Fatal(err)
		} else if !exist {
			t.Errorf("rule %s/%s/%s doesn't exist", rule.table, rule.chain, strings.Join(rule.args, " "))
		}
	}

	// Delete SNAT rule
	if err := iptr.DelSNATRule(); err != nil {
		t.Fatal(err)
	}

	// Check that the rule was deleted for ipt4 and ipt6
	for _, proto := range []iptablesInterface{iptr.ipt4, iptr.ipt6} {
		if exist, err := proto.Exists(rule.table, rule.chain, rule.args...); err != nil {
			t.Fatal(err)
		} else if exist {
			t.Errorf("rule %s/%s/%s still exists", rule.table, rule.chain, strings.Join(rule.args, " "))
		}
	}

	if err := iptr.DelChains(); err != nil {
		t.Fatal(err)
	}
}
