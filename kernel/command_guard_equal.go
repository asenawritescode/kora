package kernel

import (
	"fmt"
	"math/big"
	"strings"

	"github.com/asenawritescode/kora/contract"
)

func (c *CommandResource) validateEqualGuard(index int, operands []string) error {
	if len(operands) != 2 {
		return fmt.Errorf("command %q: require %d equal needs exactly two operands", c.FullName(), index)
	}
	for _, op := range operands {
		switch {
		case strings.HasPrefix(op, "$computed."):
			if !c.hasComputeKey(strings.TrimPrefix(op, "$computed.")) {
				return fmt.Errorf("command %q: require %d equal operand %q must name a compute key", c.FullName(), index, op)
			}
		case strings.HasPrefix(op, "$input."):
		case strings.HasPrefix(op, "$"):
			return fmt.Errorf("command %q: require %d equal operand %q must be $computed, $input or a number", c.FullName(), index, op)
		default:
			if _, err := toRat(op); err != nil {
				return fmt.Errorf("command %q: require %d equal operand %q must be $computed, $input or a number", c.FullName(), index, op)
			}
		}
	}
	return nil
}

// checkEqualGuard compares both operands as exact decimals. A mismatch or a
// non-numeric operand is a caller-facing validation failure.
func checkEqualGuard(guard CommandGuard, env refEnv, index int) *contract.Error {
	var rats [2]*big.Rat
	for i, operand := range guard.Equal[:2] {
		raw, err := resolveEnvRef(operand, env)
		if err != nil {
			return contract.NewError(contract.CodeValidationFailed, fmt.Sprintf("require %d: %v", index, err))
		}
		if rats[i], err = toRat(raw); err != nil {
			return contract.NewError(contract.CodeValidationFailed, fmt.Sprintf("require %d: %s: %v", index, operand, err))
		}
	}
	if rats[0].Cmp(rats[1]) == 0 {
		return nil
	}
	message := guard.Message
	if message == "" {
		message = fmt.Sprintf("require %d: %s (%s) must equal %s (%s)", index, guard.Equal[0], formatRat(rats[0]), guard.Equal[1], formatRat(rats[1]))
	}
	return contract.NewError(contract.CodeValidationFailed, message)
}
