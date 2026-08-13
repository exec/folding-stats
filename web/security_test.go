package web

import (
	"os/exec"
	"testing"
)

const prototypePollutionDriver = `
import { applyUpdate, MachineState } from './fah.mjs';

let bad = 0;
const check = (ok, message) => { if (!ok) { console.log('FAIL ' + message); bad++; } };
const root = {};
applyUpdate(root, ['__proto__', 'polluted', 'yes']);
applyUpdate(root, ['constructor', 'prototype', 'polluted', 'yes']);
check(({}).polluted === undefined, 'patch polluted Object.prototype');

const machine = new MachineState();
machine.accept(JSON.parse('{"__proto__":{"polluted":"yes"},"info":{"hostname":"safe"}}'));
check(({}).polluted === undefined, 'full-state message polluted Object.prototype');
check(machine.name === 'safe', 'safe full-state keys were discarded');
applyUpdate(root, ['info', 'hostname', 'patched']);
check(root.info.hostname === 'patched', 'ordinary patch stopped working');
if (!bad) console.log('OK');
`

func TestMachineMessagesCannotPollutePrototypes(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	runJSDriver(t, node, prototypePollutionDriver)
}
