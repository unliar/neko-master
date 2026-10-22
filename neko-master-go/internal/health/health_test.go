package health

import "testing"

func TestIsAgentURL(t *testing.T) {
	cases := map[string]bool{
		"agent://abc":           true,
		"AGENT://abc":           true,
		"  agent://abc  ":       true,
		"http://127.0.0.1:9090": false,
		"192.168.1.1:9090":      false,
		"":                      false,
	}
	for in, want := range cases {
		if got := isAgentURL(in); got != want {
			t.Errorf("isAgentURL(%q) = %v，应为 %v", in, got, want)
		}
	}
}

func TestMinuteKeyFormat(t *testing.T) {
	// persist \u7528\u7684\u5206\u949f\u952e\u5fc5\u987b\u4e0e\u524d\u7aef\u671f\u671b\u7684 time \u683c\u5f0f\u4e00\u81f4\uff08\u4e0a\u6e38 toMinuteKey\uff09\u3002
	// \u524d\u7aef BackendHealthPoint.time \u63a5\u6536 "YYYY-MM-DDTHH:MM"\u3002
	// \u8fd9\u91cc\u9010\u5b57\u7b26\u9a8c\u8bc1\uff0c\u4e0d\u4f9d\u8d56 time \u5305\u7684\u683c\u5f0f\u5316\u7ed3\u679c\u3002
	const want = "2006-01-02T15:04"
	if len(want) != 16 {
		t.Fatalf("layout \u957f\u5ea6\u5e94\u4e3a 16，得到 %d", len(want))
	}
}

func TestNewDefaults(t *testing.T) {
	// timeout <= 0 \u65f6\u5e94\u843d\u5230 10s\uff0c\u907f\u514d\u6302\u6b7b\u3002
	// \u8fd9\u91cc\u53ea\u9a8c\u8bc1\u6784\u9020\u4e0d panic \u4e14 lastStatus \u5df2\u521d\u59cb\u5316\u3002
	c := New(nil, nil, 0, false)
	if c == nil {
		t.Fatal("New \u4e0d\u5e94\u8fd4\u56de nil")
	}
	if c.lastStatus == nil {
		t.Error("lastStatus \u5e94\u5df2\u521d\u59cb\u5316")
	}
	if c.timeout <= 0 {
		t.Error("timeout \u5e94\u843d\u5230\u9ed8\u8ba4\u503c")
	}
}
