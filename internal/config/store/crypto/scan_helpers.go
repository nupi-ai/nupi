package crypto

type rowScanner interface {
	Scan(dest ...any) error
}

func scanStringPair(scanner rowScanner) (string, string, error) {
	var first, second string
	err := scanner.Scan(&first, &second)
	return first, second, err
}

func scanInt64StringPair(scanner rowScanner) (int64, string, error) {
	var first int64
	var second string
	err := scanner.Scan(&first, &second)
	return first, second, err
}
