package crypto

import "github.com/nupi-ai/nupi/internal/config/store/dbutil"

func scanStringPair(scanner dbutil.RowScanner) (string, string, error) {
	var first, second string
	err := scanner.Scan(&first, &second)
	return first, second, err
}

func scanInt64StringPair(scanner dbutil.RowScanner) (int64, string, error) {
	var first int64
	var second string
	err := scanner.Scan(&first, &second)
	return first, second, err
}
