package sessions

import "testing"

func TestLocalDir(t *testing.T) {
	cases := []struct{ in, want string }{
		{`C:\Users\u\AppData\Local\Temp\cc-1`, "/mnt/c/Users/u/AppData/Local/Temp/cc-1"},
		{`D:\proj`, "/mnt/d/proj"},
		{`\\wsl.localhost\Ubuntu-24.04\home\u\p`, "/home/u/p"},
		{"/home/u/p", "/home/u/p"},
	}
	for _, c := range cases {
		if got := LocalDir(c.in); got != c.want {
			t.Errorf("LocalDir(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
