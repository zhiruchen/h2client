package h2client

type flow struct {
	n    int32
	conn *flow
}

func (f *flow) setConnflow(cf *flow) {
	f.conn = cf
}

func (f *flow) available() int32 {
	n := f.n
	if f.conn != nil && f.conn.n < n {
		n = f.conn.n
	}

	return n
}

func (f *flow) take(n int32) {
	if n > f.available() {
		panic("flow error: took too much")
	}

	f.n -= n
	if f.conn != nil {
		f.conn.n -= n
	}
}

func (f *flow) add(n int32) bool {
	sum := f.n + n
	if (sum > n) == (f.n > 0) {
		f.n = sum
		return true
	}

	return false
}
