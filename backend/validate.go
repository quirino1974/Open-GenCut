// Open-GenCut - Validazioni condivise: importi, interi e lunghezze.
//
// Limiti dei tipi SQL Server usati dal database:
//
//	DECIMAL(18,4) -> importi fino a 99999999999999.9999
//	INT           -> interi fino a 2147483647
//
// Un valore fuori limite produrrebbe un errore di overflow nel database: va rifiutato prima, con 400.
package main

import (
	"math"
	"unicode/utf8"
)

const (
	maxMoney = 99999999999999.0 // DECIMAL(18,4): 14 cifre intere
	maxInt32 = 2147483647       // INT
)

// validMoney accetta importi finiti, non negativi e entro il limite di DECIMAL(18,4).
func validMoney(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 && v <= maxMoney
}

// runeLen conta i caratteri, non i byte: "è" vale 1, come nelle colonne NVARCHAR di SQL Server.
func runeLen(s string) int {
	return utf8.RuneCountInString(s)
}
