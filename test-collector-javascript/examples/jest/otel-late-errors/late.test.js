test('rejects after test_done during afterAll', () => {
  setTimeout(() => Promise.reject(new Error('late rejection during afterAll')), 50)
})

afterAll(async () => {
  process.stdout.write(`afterAll started at ${Date.now()}\n`)
  await new Promise((resolve) => setTimeout(resolve, 300))
})
