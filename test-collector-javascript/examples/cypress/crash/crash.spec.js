describe('Renderer crash', () => {
  it('completed before crash', () => { expect(2 + 7).to.equal(9) })
  it('crashes renderer', () => {
    cy.then(() => Cypress.automation('remote:debugger:protocol', { command: 'Page.crash' }))
  })
  it('never reached', () => { expect(true).to.equal(true) })
})
