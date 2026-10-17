describe('outcomes', () => {
  it('passes with a frozen browser clock', () => {
    cy.clock(0)
    cy.wait(50)
    expect(true).to.equal(true)
  })

  it('fails', () => {
    expect(2).to.equal(3)
  })

  it.skip('is skipped', () => {})

  it('passes on retry', { retries: 1 }, function () {
    expect(this.test.currentRetry()).to.equal(1)
  })
})

describe('before hook failure', () => {
  before(() => { throw new Error('before hook failed') })
  it('cannot run', () => {})
  it('is skipped after the hook failure', () => {})
})

describe('after hook failure', () => {
  afterEach(() => { throw new Error('after hook failed') })
  it('fails during teardown', () => {})
})
