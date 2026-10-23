test('empty passing body', () => {})
test('empty failing body', () => { throw new Error('empty body failure') })
describe('empty failing setup', () => {
  beforeEach(() => { throw new Error('empty setup failure') })
  test('never runs', () => {})
})
describe('empty failing teardown', () => {
  afterEach(() => { throw new Error('empty teardown failure') })
  test('body passed', () => {})
})
