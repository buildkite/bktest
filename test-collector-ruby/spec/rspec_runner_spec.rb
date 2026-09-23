# frozen_string_literal: true

require "socket"
require "open3"
require "timeout"

RSpec.describe "buildkite-rspec" do
  let(:executable) { File.expand_path("../exe/buildkite-rspec", __dir__) }
  let(:lib) { File.expand_path("../lib", __dir__) }

  around do |example|
    Dir.mktmpdir("buildkite-rspec") do |dir|
      @dir = dir
      Dir.mkdir("#{dir}/spec")
      File.write("#{dir}/spec/rails_helper.rb", <<~RUBY)
        require "buildkite/test_collector"
        Buildkite::TestCollector.configure(hook: :rspec, token: nil)
        File.open("boots", "a") { |f| f.puts(Process.pid) }
        class << Buildkite::TestCollector::Uploader
          def upload(traces)
            File.open("executions", "a") do |f|
              traces.each { |t| f.puts(JSON.generate(t.as_hash)) }
            end
            nil
          end
        end
        RSpec.configure do |c|
          c.order = :defined
          c.before(:suite) do
            File.open("sessions", "a") { |f| f.puts(Buildkite::TestCollector.session.object_id) }
          end
        end
      RUBY
      File.write("#{dir}/spec/sample_spec.rb", <<~RUBY)
        RSpec.describe "sample" do
          it("flaky") do
            $attempts = ($attempts || 0) + 1
            expect($attempts).to be > 1
          end
          it("passing") { expect(2 + 3).to eq(5) }
        end
      RUBY
      @server = UNIXServer.new("#{dir}/runner.sock")
      example.run
    ensure
      @server&.close
    end
  end

  def with_runner(*args, env: {})
    environment = { "BUILDKITE_TEST_ENGINE_RUNNER_SOCKET" => "#{@dir}/runner.sock",
                    "BUILDKITE_ANALYTICS_TOKEN" => nil, "SPEC_OPTS" => nil,
                    "XDG_CONFIG_HOME" => @dir, "HOME" => @dir }.merge(env)
    Open3.popen3(environment, RbConfig.ruby, "-I", lib, executable, *args, chdir: @dir) do |stdin, stdout, stderr, child|
      stdin.close
      out = Thread.new { stdout.read }
      err = Thread.new { stderr.read }
      begin
        Timeout.timeout(15) { yield child }
        status = Timeout.timeout(15) { child.value }
        @output = out.value + err.value
        status
      ensure
        unless child.join(0)
          Process.kill("KILL", child.pid)
          child.join
        end
      end
    end
  end

  def request(response = {}, status: 200, disconnect: false)
    socket = @server.accept
    method, path = socket.gets.split(" ")
    headers = {}
    while (line = socket.gets) != "\r\n"
      key, value = line.strip.split(": ", 2)
      headers[key.downcase] = value
    end
    raw = socket.read(headers.fetch("content-length").to_i)
    body = JSON.parse(raw)
    yield method, path, headers, body, raw if block_given?
    unless disconnect
      encoded = JSON.generate(response)
      socket.write("HTTP/1.1 #{status} OK\r\nContent-Type: application/json\r\nContent-Length: #{encoded.bytesize}\r\nConnection: close\r\n\r\n#{encoded}")
    end
    body
  ensure
    socket&.close
  end

  def handshake
    request({ session_id: "session-1", poll: { max_wait_ms: 30_000 } }, status: 201) do |method, path, headers, body|
      expect([method, path]).to eq(["POST", "/v1/sessions"])
      expect(headers).not_to have_key("x-bktec-session")
      expect(File.readlines("#{@dir}/boots").size).to eq(1)
      expect(body.fetch("capabilities").fetch("selector_formats")).to eq(%w[selector file example])
      expect(body.fetch("runner").fetch("name")).to eq("buildkite-rspec")
      expect(body.fetch("runner").fetch("framework")).to eq("rspec")
    end
  end

  def dispatch(id, tests)
    request({ type: "batch", batch: { id: id, tests: tests, timeout_ms: 600_000 } }) do |method, path, headers, body|
      expect([method, path, body]).to eq(["POST", "/v1/batches", {}])
      expect(headers["x-bktec-session"]).to eq("session-1")
    end
  end

  it "boots once, isolates reports/filters/sessions and records each host retry execution once" do
    reports = []
    status = with_runner do
      handshake
      request({ type: "wait", retry_after_ms: 1 })
      dispatch("first", [{ format: "selector", value: "spec/sample_spec.rb[1:1]", path: "wrong.rb" }])
      reports << request
      dispatch("second", [{ format: "file", path: "spec/sample_spec.rb", value: "wrong.rb" }])
      reports << request
      dispatch("retry", [{ format: "example", identifier: "spec/sample_spec.rb[1:1]", path: "wrong.rb" }])
      reports << request
      dispatch("fallback", [{ format: "example", path: "spec/sample_spec.rb" }])
      reports << request
      request({ type: "done", reason: "plan_completed" })
    end
    expect(status.exitstatus).to eq(0), @output
    expect(reports.map { |r| r.fetch("status") }).to eq(["completed"] * 4)
    expect(reports.map { |r| r.fetch("report").fetch("examples").size }).to eq([1, 2, 1, 2])
    expect(reports.map { |r| r.fetch("report").fetch("summary").fetch("failure_count") }).to eq([1, 0, 0, 0])
    expect(File.readlines("#{@dir}/boots").size).to eq(1)
    expect(File.readlines("#{@dir}/sessions").uniq.size).to eq(4)
    executions = File.readlines("#{@dir}/executions").map { |line| JSON.parse(line) }
    expect(executions.size).to eq(6)
    expect(executions.map { |e| e.fetch("external_id") }.uniq.size).to eq(6)
    expect(executions.map { |e| e.fetch("result") }).to eq(%w[failed passed passed passed passed passed])
  end

  it "replays identical result bytes after a lost acknowledgment without re-executing" do
    original = nil
    status = with_runner do
      handshake
      dispatch("one", [{ format: "selector", value: "spec/sample_spec.rb[1:2]" }])
      request(disconnect: true) { |_, _, _, _, raw| original = raw }
      request { |_, path, _, _, raw| expect([path, raw]).to eq(["/v1/batches/one/results", original]) }
      request({ type: "done", reason: "plan_completed" })
    end
    expect(status.exitstatus).to eq(0), @output
    expect(File.readlines("#{@dir}/executions").size).to eq(1)
  end

  it "clears non-example failures and quit state before the next batch" do
    File.write("#{@dir}/spec/broken_spec.rb", 'raise "load failed"')
    reports = []
    status = with_runner do
      handshake
      dispatch("load-error", [{ format: "file", path: "spec/broken_spec.rb" }])
      reports << request
      dispatch("healthy", [{ format: "selector", value: "spec/sample_spec.rb[1:2]" }])
      reports << request
      request({ type: "done", reason: "plan_completed" })
    end
    expect(status.exitstatus).to eq(0), @output
    summaries = reports.map { |r| r.fetch("report").fetch("summary") }
    expect(summaries.map { |s| s.fetch("errors_outside_of_examples_count") }).to eq([1, 0])
    expect(summaries.map { |s| s.fetch("example_count") }).to eq([0, 1])
    expect(File.readlines("#{@dir}/executions").size).to eq(1)
  end

  it "sends the errored envelope and exits nonzero when RSpec leaves no report" do
    File.open("#{@dir}/spec/rails_helper.rb", "a") do |file|
      file.write(<<~RUBY)
        module RemoveReport
          def run(args, *rest)
            result = super
            File.unlink(args[args.index("--out") + 1])
            result
          end
        end
        RSpec::Core::Runner.singleton_class.prepend(RemoveReport)
      RUBY
    end
    result = nil
    status = with_runner do
      handshake
      dispatch("missing", [{ format: "file", path: "spec/sample_spec.rb" }])
      result = request
    end
    expect(status.exitstatus).to eq(1), @output
    expect(result).to include("status" => "errored", "report_format" => "rspec-json")
    expect(result.fetch("error")).to include("kind" => "runner_error")
  end

  %w[INT TERM].each do |signal|
    it "finishes the current example on SIG#{signal}, flushes and DELETEs without posting a partial report" do
      File.write("#{@dir}/spec/sample_spec.rb", <<~RUBY)
        RSpec.describe "signal" do
          it("current") do
            File.write("started", "yes")
            sleep(0.01) until File.exist?("continue")
            File.write("finished", "yes")
          end
          it("never run") { File.write("unwanted", "yes") }
        end
      RUBY
      status = with_runner do |child|
        handshake
        dispatch("interrupted", [{ format: "file", path: "spec/sample_spec.rb" }])
        sleep(0.01) until File.exist?("#{@dir}/started")
        Process.kill(signal, child.pid)
        sleep(0.05)
        File.write("#{@dir}/continue", "yes")
        request do |method, path, _, body|
          expect([method, path]).to eq(["DELETE", "/v1/sessions/session-1"])
          expect(body).to eq("reason" => "signal_#{signal.downcase}")
        end
      end
      expect(status.exitstatus).to eq(1), @output
      expect(File.exist?("#{@dir}/finished")).to be(true)
      expect(File.exist?("#{@dir}/unwanted")).to be(false)
      expect(File.readlines("#{@dir}/executions").size).to eq(1)
    end
  end

  it "keeps CLI formatters separate from native JSON reports across batches" do
    reports = []
    status = with_runner("--format", "documentation", "--format", "progress", "--out", "progress.txt",
                         "--format", "json", "--out", "caller.json") do
      handshake
      2.times do |index|
        dispatch("batch-#{index}", [{ format: "example", identifier: "spec/sample_spec.rb[1:2]" }])
        reports << request
      end
      request({ type: "done", reason: "plan_completed" })
    end
    expect(status.exitstatus).to eq(0), @output
    expect(@output.scan("1 example, 0 failures").size).to eq(2)
    expect(@output.scan(/^  passing$/).size).to eq(2)
    expect(File.read("#{@dir}/progress.txt")).to include("1 example, 0 failures")
    expect(JSON.parse(File.read("#{@dir}/caller.json"))).to eq(reports.last.fetch("report"))
    expect(reports.map { |r| r.fetch("report").fetch("examples").size }).to eq([1, 1])
    expect(File.readlines("#{@dir}/executions").size).to eq(2)
  end

  [".rspec", ".rspec.ci"].each do |options_file|
    it "loads requires and formatters from #{options_file} without replacing the native report" do
      File.write("#{@dir}/support.rb", 'File.write("required", "yes")')
      File.write("#{@dir}/#{options_file}", "--require ./support.rb\n--format documentation\n")
      args = options_file == ".rspec" ? [] : ["--options", options_file]
      # A custom options file must replace the default, not merely add to it.
      File.write("#{@dir}/.rspec", "--dry-run\n") unless args.empty?
      result = nil
      status = with_runner(*args) do
        handshake
        expect(File.read("#{@dir}/required")).to eq("yes")
        dispatch("formatted", [{ format: "example", identifier: "spec/sample_spec.rb[1:2]" }])
        result = request
        request({ type: "done", reason: "plan_completed" })
      end
      expect(status.exitstatus).to eq(0), @output
      expect(@output).to include("passing", "1 example, 0 failures")
      expect(result.fetch("report").fetch("examples").size).to eq(1)
      expect(File.readlines("#{@dir}/executions").size).to eq(1)
    end
  end

  it "rejects caller selectors and persistent-incompatible options before boot" do
    status = with_runner("spec/sample_spec.rb") { }
    expect(status.exitstatus).to eq(1)
    expect(@output).to include("caller selectors")
    File.write("#{@dir}/.rspec", "--dry-run\n")
    status = with_runner { }
    expect(status.exitstatus).to eq(1)
    expect(@output).to include("--dry-run is incompatible", "assigned tests must execute")
    expect(File.exist?("#{@dir}/boots")).to be(false)
  end

  it "rejects dry-run from the CLI or a custom options file before boot" do
    status = with_runner("--dry-run") { }
    expect(status.exitstatus).to eq(1)
    expect(@output).to include("--dry-run is incompatible")
    File.write("#{@dir}/.rspec.ci", "--dry-run\n")
    status = with_runner("--options", ".rspec.ci") { }
    expect(status.exitstatus).to eq(1)
    expect(@output).to include("--dry-run is incompatible")
    expect(File.exist?("#{@dir}/boots")).to be(false)
  end

  it "interrupts an idle long poll and DELETEs instead of retrying for more work" do
    status = with_runner do |child|
      handshake
      request(disconnect: true) do |method, path|
        expect([method, path]).to eq(["POST", "/v1/batches"])
        Process.kill("TERM", child.pid)
        sleep(0.05)
      end
      request do |method, path|
        expect([method, path]).to eq(["DELETE", "/v1/sessions/session-1"])
      end
    end
    expect(status.exitstatus).to eq(1), @output
    expect(File.exist?("#{@dir}/executions")).to be(false)
  end

  it "fails closed when a session is fenced" do
    status = with_runner do
      handshake
      request({}, status: 401)
    end
    expect(status.exitstatus).to eq(1), @output
    expect(@output).to include("HTTP 401")
  end

  it "rejects unknown done reasons but leaves host job outcomes to bktec" do
    status = with_runner do
      handshake
      request({ type: "done", reason: "new-unknown-reason" })
    end
    expect(status.exitstatus).to eq(1), @output
    expect(@output).to include("unknown done reason")
    File.delete("#{@dir}/boots")
    status = with_runner do
      handshake
      request({ type: "done", reason: "pool_errored" })
    end
    expect(status.exitstatus).to eq(0), @output
  end
end
