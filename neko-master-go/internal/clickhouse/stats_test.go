package clickhouse

import "testing"

func TestParseInt64(t *testing.T) {
	cases := []struct {
		in   string
		want int64
	}{
		{"", 0}, // sum() \u65e0\u5339\u914d\u884c\u65f6 CH \u8fd4\u56de\u7a7a
		{"0", 0},
		{"12345", 12345},
		{" 42 ", 0}, // caller trims; parseInt64 itself does not
		{"1.8446744073709552e+18", 1844674407370955264}, // float64 rounding
		{"not-a-number", 0},
	}
	for _, c := range cases {
		if got := parseInt64(c.in); got != c.want {
			t.Errorf("parseInt64(%q) = %d\uff0c\u5e94\u4e3a %d", c.in, got, c.want)
		}
	}
}

func TestStorageQueriesNilWriterSafe(t *testing.T) {
	// Writer \u4e3a nil \u65f6\u8c03\u7528\u65b9\u5fc5\u987b\u5148\u5224\u7a7a\uff0c\u8fd9\u91cc\u786e\u8ba4\u65b9\u6cd5\u672c\u8eab\u4e0d\u4f1a\u56e0 nil receiver panic
	// \uff08Enabled() \u5df2\u505a nil \u4fdd\u62a4\uff0c\u4f46\u65b9\u6cd5\u4e0a\u7684\u884c\u4e3a\u4e5f\u5e94\u53ef\u9884\u671f\uff09
	var w *Writer
	if w.Enabled() {
		t.Error("nil Writer \u5e94\u62a5\u544a\u672a\u542f\u7528")
	}
}
