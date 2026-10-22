package stats

import (
	"sort"
	"testing"
)

func TestSplitChainParts(t *testing.T) {
	got := splitChainParts("A > B > C")
	want := []string{"A", "B", "C"}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	// \u7a7a\u6bb5\u4e0e\u7a7a\u767d\u5e94\u88ab\u8fc7\u6ee4
	if got := splitChainParts("A >  > B > "); len(got) != 2 {
		t.Errorf("\u7a7a\u6bb5\u672a\u8fc7\u6ee4: %v", got)
	}
}

// \u5173\u952e\uff1a\u5b58\u50a8\u7684 chain \u662f proxy\u2192...\u2192rule\uff0c\u5fc5\u987b\u53cd\u8f6c\u6210 rule\u2192...\u2192proxy\u3002
func TestBuildRuleFlowPathReverses(t *testing.T) {
	got := buildRuleFlowPath("MyRule", "FinalProxy > Group > MyRule")
	want := []string{"MyRule", "Group", "FinalProxy"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] = %q, want %q (\u5168\u5f62 %v)", i, got[i], want[i], got)
		}
	}
}

// rule \u4e0d\u5728 chain \u91cc\uff08\u5982 minute_dim \u7684\u884c\uff09\u65f6\u524d\u7f6e rule\u3002
func TestBuildRuleFlowPathPrependsRule(t *testing.T) {
	got := buildRuleFlowPath("MyRule", "FinalProxy > Group")
	if len(got) != 3 {
		t.Fatalf("\u957f\u5ea6\u5e94\u4e3a 3, got %v", got)
	}
	if got[0] != "MyRule" {
		t.Errorf("\u9996\u5143\u7d20\u5e94\u4e3a rule, got %q", got[0])
	}
	if got[1] != "Group" || got[2] != "FinalProxy" {
		t.Errorf("\u540e\u7eed\u5143\u7d20\u5e94\u4e3a chain \u53cd\u8f6c, got %v", got)
	}
}

func TestBuildRuleFlowPathEmptyChain(t *testing.T) {
	got := buildRuleFlowPath("MyRule", "")
	if len(got) != 1 || got[0] != "MyRule" {
		t.Errorf("got %v", got)
	}
}

func TestNormalizeFlowLabel(t *testing.T) {
	cases := map[string]string{
		"  Hello   World ": "hello world",
		"**DIRECT**":       "direct**",
		"ABC":              "abc",
	}
	for in, want := range cases {
		if got := normalizeFlowLabel(in); got != want {
			t.Errorf("normalizeFlowLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIsBuiltInPolicy(t *testing.T) {
	for _, n := range []string{"DIRECT", "REJECT", "REJECT-TINY"} {
		if !isBuiltInPolicy(n) {
			t.Errorf("%q \u5e94\u4e3a\u5185\u7f6e\u7b56\u7565", n)
		}
	}
	for _, n := range []string{"MyProxy", "", "direct"} {
		if isBuiltInPolicy(n) {
			t.Errorf("%q \u4e0d\u5e94\u4e3a\u5185\u7f6e\u7b56\u7565", n)
		}
	}
}

func TestSplitLinkKeyRoundTrip(t *testing.T) {
	src, tgt := splitLinkKey("A\x00B")
	if src != "A" || tgt != "B" {
		t.Errorf("got %q / %q", src, tgt)
	}
	// \u542b\x00\u7684\u540d\u5b57\u672c\u9879\u76ee\u4e0d\u51fa\u73b0\uff0c\u4f46\u8981\u786e\u8ba4\u4e0d panic
	s2, t2 := splitLinkKey("no-sep")
	if s2 != "no-sep" || t2 != "" {
		t.Errorf("got %q / %q", s2, t2)
	}
}

// \u8282\u70b9\u7c7b\u578b\u5224\u5b9a\uff1a\u65e0\u5165\u8fb9=rule\uff08layer \u56fa\u5b9a 0\uff09\u3001
// \u65e0\u51fa\u8fb9\u4e14\u975e\u5185\u7f6e=proxy\uff08\u63a8\u5230\u6700\u53f3\uff09\u3001\u5176\u4f59=group\u3002
func TestBuildChainFlowGraphNodeTypes(t *testing.T) {
	rows := []flowRow{
		{Rule: "R1", Chain: "FinalProxy > MidGroup > R1", TotalUpload: 100, TotalDown: 200, TotalConns: 3},
	}
	g := buildChainFlowGraph(rows)

	byName := map[string]ChainFlowNode{}
	for _, n := range g.Nodes {
		byName[n.Name] = n
	}

	if n := byName["R1"]; n.NodeType != "rule" || n.Layer != 0 {
		t.Errorf("R1 \u5e94\u4e3a rule/layer0, got %s/%d", n.NodeType, n.Layer)
	}
	if n := byName["MidGroup"]; n.NodeType != "group" {
		t.Errorf("MidGroup \u5e94\u4e3a group, got %s", n.NodeType)
	}
	if n := byName["FinalProxy"]; n.NodeType != "proxy" {
		t.Errorf("FinalProxy \u5e94\u4e3a proxy, got %s", n.NodeType)
	}

	// proxy \u5e94\u88ab\u63a8\u5230\u6700\u53f3\u5217
	if byName["FinalProxy"].Layer != g.MaxLayer {
		t.Errorf("proxy layer = %d, maxLayer = %d", byName["FinalProxy"].Layer, g.MaxLayer)
	}

	// \u6d41\u91cf\u5e94\u7d2f\u52a0\u5230\u8def\u5f84\u4e0a\u6bcf\u4e2a\u8282\u70b9
	for _, n := range g.Nodes {
		if n.TotalUpload != 100 || n.TotalDownload != 200 || n.TotalConnections != 3 {
			t.Errorf("\u8282\u70b9 %s \u6d41\u91cf = %d/%d/%d", n.Name, n.TotalUpload, n.TotalDownload, n.TotalConnections)
		}
	}
}

// DIRECT \u662f\u5185\u7f6e\u7b56\u7565\uff0c\u65e0\u51fa\u8fb9\u4e5f\u4e0d\u5f53 proxy\uff0c\u5e94\u4e3a group\u3002
func TestDirectNotClassifiedAsProxy(t *testing.T) {
	rows := []flowRow{
		{Rule: "R1", Chain: "DIRECT > R1", TotalUpload: 1, TotalDown: 1, TotalConns: 1},
	}
	g := buildChainFlowGraph(rows)
	for _, n := range g.Nodes {
		if n.Name == "DIRECT" && n.NodeType == "proxy" {
			t.Error("DIRECT \u4e0d\u5e94\u88ab\u5f52\u4e3a proxy")
		}
	}
}

// links \u7684 source/target \u5fc5\u987b\u662f\u6570\u5b57\u4e0b\u6807\uff0c\u4e14\u5728\u8282\u70b9\u8303\u56f4\u5185\u3002
func TestLinksAreIndices(t *testing.T) {
	rows := []flowRow{
		{Rule: "R1", Chain: "P > G > R1", TotalUpload: 10, TotalDown: 10, TotalConns: 1},
		{Rule: "R2", Chain: "P2 > R2", TotalUpload: 5, TotalDown: 5, TotalConns: 1},
	}
	g := buildChainFlowGraph(rows)

	if len(g.Links) == 0 {
		t.Fatal("\u5e94\u81f3\u5c11\u6709\u8fb9")
	}
	for _, l := range g.Links {
		if l.Source < 0 || l.Source >= len(g.Nodes) {
			t.Errorf("source \u8d8a\u754c: %d", l.Source)
		}
		if l.Target < 0 || l.Target >= len(g.Nodes) {
			t.Errorf("target \u8d8a\u754c: %d", l.Target)
		}
	}
}

// rulePaths \u5fc5\u987b\u6b63\u786e\u6307\u5411\u8282\u70b9\u548c\u8fb9\u4e0b\u6807\u3002
func TestRulePathsIndices(t *testing.T) {
	rows := []flowRow{
		{Rule: "R1", Chain: "P > G > R1", TotalUpload: 10, TotalDown: 10, TotalConns: 1},
	}
	g := buildChainFlowGraph(rows)

	rp, ok := g.RulePaths["R1"]
	if !ok {
		t.Fatal("\u7f3a R1 \u7684 rulePath")
	}
	if len(rp.NodeIndices) != 3 {
		t.Errorf("R1 \u5e94\u7ecf\u8fc7 3 \u4e2a\u8282\u70b9, got %d", len(rp.NodeIndices))
	}
	if len(rp.LinkIndices) != 2 {
		t.Errorf("R1 \u5e94\u7ecf\u8fc7 2 \u6761\u8fb9, got %d", len(rp.LinkIndices))
	}
	// \u4e0b\u6807\u5fc5\u987b\u5347\u5e8f\u4e14\u5408\u6cd5
	if !sort.IntsAreSorted(rp.NodeIndices) {
		t.Errorf("nodeIndices \u672a\u6392\u5e8f: %v", rp.NodeIndices)
	}
	for _, i := range rp.LinkIndices {
		if i < 0 || i >= len(g.Links) {
			t.Errorf("linkIndex \u8d8a\u754c: %d", i)
		}
	}
}

// flowPath \u957f\u5ea6 < 2 \u7684\u884c\u5e94\u88ab\u8df3\u8fc7\uff08\u4e0a\u6e38 continue\uff09\u3002
func TestShortFlowPathSkipped(t *testing.T) {
	rows := []flowRow{
		{Rule: "R1", Chain: "", TotalUpload: 10, TotalDown: 10, TotalConns: 1},
	}
	g := buildChainFlowGraph(rows)
	// chain \u4e3a\u7a7a\u65f6 buildRuleFlowPath \u8fd4\u56de [rule]\uff0c\u957f\u5ea6 1 < 2 \u2192 \u8df3\u8fc7
	if len(g.Nodes) != 0 {
		t.Errorf("\u5e94\u4e3a\u7a7a\u56fe, got %d nodes", len(g.Nodes))
	}
}

func TestEmptyInput(t *testing.T) {
	g := buildChainFlowGraph(nil)
	if g == nil {
		t.Fatal("\u4e0d\u5e94\u8fd4\u56de nil")
	}
	if len(g.Nodes) != 0 || len(g.Links) != 0 || len(g.RulePaths) != 0 {
		t.Error("\u7a7a\u8f93\u5165\u5e94\u5f97\u7a7a\u56fe")
	}
	if g.MaxLayer != 0 {
		t.Errorf("maxLayer \u5e94\u4e3a 0, got %d", g.MaxLayer)
	}
}

// \u591a\u6761\u89c4\u5219\u5171\u4eab\u8282\u70b9\u65f6\uff0c\u6d41\u91cf\u4e0e rules \u5e94\u5408\u5e76\u3002
func TestSharedNodeMergesTraffic(t *testing.T) {
	rows := []flowRow{
		{Rule: "R1", Chain: "P > R1", TotalUpload: 10, TotalDown: 20, TotalConns: 1},
		{Rule: "R2", Chain: "P > R2", TotalUpload: 30, TotalDown: 40, TotalConns: 2},
	}
	g := buildChainFlowGraph(rows)

	var p *ChainFlowNode
	for i := range g.Nodes {
		if g.Nodes[i].Name == "P" {
			p = &g.Nodes[i]
			break
		}
	}
	if p == nil {
		t.Fatal("\u7f3a\u5171\u4eab\u8282\u70b9 P")
	}
	if p.TotalUpload != 40 || p.TotalDownload != 60 || p.TotalConnections != 3 {
		t.Errorf("P \u5408\u5e76\u540e = %d/%d/%d", p.TotalUpload, p.TotalDownload, p.TotalConnections)
	}
	if len(p.Rules) != 2 {
		t.Errorf("P \u5e94\u5173\u8054 2 \u6761\u89c4\u5219, got %v", p.Rules)
	}
}
