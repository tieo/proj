package sessions

import "testing"

func BenchmarkList(b *testing.B) {
	home := Home("")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := List(home); err != nil {
			b.Fatal(err)
		}
	}
}
