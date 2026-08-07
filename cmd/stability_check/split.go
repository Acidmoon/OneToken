package main

import "onetoken/internal/store"

func splitHalf(fp *store.Fingerprint) (*store.Fingerprint, *store.Fingerprint) {
	a, b := &store.Fingerprint{Cells: map[string]store.CellDist{}}, &store.Fingerprint{Cells: map[string]store.CellDist{}}
	for cell, cd := range fp.Cells {
		da, db := map[string]int{}, map[string]int{}
		i := 0
		for k, v := range cd.Dist {
			for j := 0; j < v; j++ {
				if (i+j)%2 == 0 {
					da[k]++
				} else {
					db[k]++
				}
			}
			i += v
		}
		na, nb := 0, 0
		for _, v := range da {
			na += v
		}
		for _, v := range db {
			nb += v
		}
		if na >= 10 && nb >= 10 {
			a.Cells[cell] = store.CellDist{Dist: da, N: na, T: 1.0}
			b.Cells[cell] = store.CellDist{Dist: db, N: nb, T: 1.0}
		}
	}
	return a, b
}
