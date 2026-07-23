package codexreg

import (
	cryptorand "crypto/rand"
	"math/big"
	"strconv"
)

var firstNames = []string{"Alex", "Jamie", "Taylor", "Jordan", "Casey", "Morgan", "Riley", "Avery", "Quinn", "Parker", "Cameron", "Reese", "Skyler", "Drew", "Emerson"}
var lastNames = []string{"Ray", "Lee", "Cole", "Reed", "Hunt", "Ford", "Shaw", "Gray", "Vance", "Wolfe", "Brooks", "Hayes", "Pierce", "Quinn", "Sloan"}

func ri(n int) int {
	if n <= 0 {
		return 0
	}
	v, err := cryptorand.Int(cryptorand.Reader, big.NewInt(int64(n)))
	if err != nil {
		return 0
	}
	return int(v.Int64())
}

// genName 随机英文姓名。
func genName() string {
	return firstNames[ri(len(firstNames))] + " " + lastNames[ri(len(lastNames))]
}

// genAge 随机成年年龄（18-45）。
func genAge() string {
	return strconv.Itoa(18 + ri(28))
}

// GenPassword 生成满足强度要求（大小写+数字）的随机密码，供 producer 复用。
func GenPassword(n int) string {
	const lower = "abcdefghijkmnpqrstuvwxyz"
	const upper = "ABCDEFGHJKLMNPQRSTUVWXYZ"
	const digit = "23456789"
	all := lower + upper + digit
	if n < 12 {
		n = 12
	}
	b := make([]byte, n)
	b[0] = upper[ri(len(upper))]
	b[1] = lower[ri(len(lower))]
	b[2] = digit[ri(len(digit))]
	for i := 3; i < n; i++ {
		b[i] = all[ri(len(all))]
	}
	return string(b)
}
