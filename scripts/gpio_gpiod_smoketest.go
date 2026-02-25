package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"gatewayd/endpoints/drivers/gpio_gpiod"
)

func main() {
	var (
		id         = flag.String("id", "gpio_gpiod_test", "endpoint id / interface_id")
		chip       = flag.String("chip", "gpiochip0", "gpio chip name or /dev/gpiochipX")
		line       = flag.Int("line_offset", -1, "line offset (required)")
		activeHigh = flag.Bool("active_high", true, "active high")
		pulseMS    = flag.Int("pulse_ms", 0, "optional pulse ms")
		debounceMS = flag.Int("debounce_ms", 0, "optional debounce ms")
		holdMS     = flag.Int("hold_ms", 250, "ms to hold PTTDown before PTTUp")
	)
	flag.Parse()

	if *line < 0 {
		fmt.Println("ERROR: -line_offset is required and must be >= 0")
		os.Exit(2)
	}

	cfg := map[string]any{
		"chip":        *chip,
		"line_offset": *line,
		"active_high": *activeHigh,
		"pulse_ms":    *pulseMS,
		"debounce_ms": *debounceMS,
	}

	ep, err := gpio_gpiod.New(*id, "gpio", "gpio_gpiod", cfg)
	if err != nil {
		fmt.Printf("ERROR: New(): %v\n", err)
		os.Exit(1)
	}

	ctx := context.Background()

	if err := ep.Connect(ctx); err != nil {
		fmt.Printf("ERROR: Connect(): %v\n", err)
		os.Exit(1)
	}
	defer ep.Disconnect(ctx)

	// Ensure starts deasserted.
	startAsserted, err := ep.ReadAsserted(ctx)
	if err != nil {
		fmt.Printf("ERROR: ReadAsserted(start): %v\n", err)
		os.Exit(1)
	}
	if startAsserted {
		fmt.Println("ERROR: expected deasserted at start, but line reads asserted=true")
		os.Exit(1)
	}

	if err := ep.PTTDown(ctx, map[string]any{"test": true}); err != nil {
		fmt.Printf("ERROR: PTTDown(): %v\n", err)
		os.Exit(1)
	}

	time.Sleep(time.Duration(*holdMS) * time.Millisecond)

	afterDown, err := ep.ReadAsserted(ctx)
	if err != nil {
		fmt.Printf("ERROR: ReadAsserted(after down): %v\n", err)
		os.Exit(1)
	}
	if !afterDown {
		fmt.Println("ERROR: expected asserted=true after PTTDown, but read asserted=false")
		os.Exit(1)
	}

	if err := ep.PTTUp(ctx, map[string]any{"test": true}); err != nil {
		fmt.Printf("ERROR: PTTUp(): %v\n", err)
		os.Exit(1)
	}

	time.Sleep(50 * time.Millisecond)

	afterUp, err := ep.ReadAsserted(ctx)
	if err != nil {
		fmt.Printf("ERROR: ReadAsserted(after up): %v\n", err)
		os.Exit(1)
	}
	if afterUp {
		fmt.Println("ERROR: expected asserted=false after PTTUp, but read asserted=true")
		os.Exit(1)
	}

	fmt.Println("OK: gpio_gpiod smoketest passed (readback verified)")
}
