# frozen_string_literal: true

module Buildkite
  module TestCollector
    module OTel
      # Wrap the instrumented app, outside OpenTelemetry's Rack middleware.
      # Only requests carrying a current local test's W3C trace context are
      # associated; headerless browser traffic must never guess an owner.
      #
      # The Rack instrumentation's default Rack::Events handler keeps the
      # server span's context attached until the response body closes, so the
      # collector's context must live exactly as long: detaching when `call`
      # returns would pop the server span's context instead, misparenting
      # spans opened while the body is read.
      class RackMiddleware
        def initialize(app)
          @app = app
        end

        def call(env)
          context = OTel.rack_context(env)
          return @app.call(env) unless context

          token = OpenTelemetry::Context.attach(context)
          fiber = Fiber.current
          begin
            status, headers, body = @app.call(env)
          rescue Exception # rubocop:disable Lint/RescueException
            detach(token, fiber)
            raise
          end
          [status, headers, ::Rack::BodyProxy.new(body) { detach(token, fiber) }]
        end

        private

        # Like the instrumentation, never pop another fiber's context stack
        # when a body is closed elsewhere.
        def detach(token, fiber)
          OpenTelemetry::Context.detach(token) if Fiber.current.equal?(fiber)
        end
      end
    end
  end
end
