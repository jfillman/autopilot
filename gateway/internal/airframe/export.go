package airframe

// PyEq is Python's ==: mappings ignore key order, lists do not, and 1 == 1.0 == True.
func PyEq(a, b any) bool { return plPyEq(a, b) }
