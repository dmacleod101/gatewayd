package factory

import (
	"fmt"

	"gatewayd/core/config"
	"gatewayd/endpoints/contract"
	"gatewayd/endpoints/drivers/gpio_gpiod"
	"gatewayd/endpoints/drivers/gpio_stub"
	"gatewayd/endpoints/drivers/mcptt_stub"
)

// Build constructs endpoint instances for enabled endpoints in config.
func Build(c *config.Config) ([]contract.Endpoint, error) {
	var eps []contract.Endpoint

	for _, ce := range c.Endpoints {
		if !ce.Enabled {
			continue
		}

		var ep contract.Endpoint
		var err error

		switch ce.Driver {
		case "mcptt_stub":
			ep, err = mcptt_stub.New(ce.ID, ce.Type, ce.Driver, ce.Config)

		case "gpio_stub":
			ep, err = gpio_stub.New(ce.ID, ce.Type, ce.Driver, ce.Config)

		case "gpio_gpiod":
			ep, err = gpio_gpiod.New(ce.ID, ce.Type, ce.Driver, ce.Config)

		default:
			return nil, fmt.Errorf("unknown endpoint driver: %s (endpoint id=%s)", ce.Driver, ce.ID)
		}

		if err != nil {
			return nil, fmt.Errorf("construct endpoint %s (%s): %w", ce.ID, ce.Driver, err)
		}

		eps = append(eps, ep)
	}

	return eps, nil
}
