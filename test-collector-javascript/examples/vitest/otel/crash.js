import { test } from 'vitest'
test('worker crashes', () => process.exit(1))
test('never reached', () => {})
