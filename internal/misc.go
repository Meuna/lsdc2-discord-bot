package internal

import (
	"crypto/hmac"
	"crypto/sha512"
	"fmt"
	"time"
)

func Pointer[T any](d T) *T {
	return &d
}

func Contains(s []string, str string) bool {
	for _, v := range s {
		if v == str {
			return true
		}
	}

	return false
}

func GenMacWithTTL(key []byte, msg []byte, ttl int) (mac []byte, eol int64) {
	eol = time.Now().Unix() + int64(ttl)
	mac = computeMac(key, msg, eol)
	return
}

func VerifyMacWithTTL(key []byte, msg []byte, eol int64, receivedMac []byte) bool {
	computedMac := computeMac(key, msg, eol)
	return hmac.Equal(computedMac, receivedMac)
}

func computeMac(key []byte, msg []byte, eol int64) []byte {
	eolBytes := []byte(fmt.Sprint(eol))
	signedMsg := append(msg, eolBytes...)

	engine := hmac.New(sha512.New, key)
	engine.Write(signedMsg)
	return engine.Sum(nil)
}
