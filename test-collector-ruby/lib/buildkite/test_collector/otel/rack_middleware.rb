# frozen_string_literal: true

module Buildkite
  module TestCollector
    module OTel
      # Wrap the instrumented app, outside OpenTelemetry's Rack middleware.
      # Only requests carrying a current local test's W3C trace context are
      # associated; headerless browser traffic must never guess an owner.
      # Ownership is detached when the body closes, not when `call` returns:
      # Rack::Events keeps the server span's context attached until body.close.
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

        # Never pop another fiber's context stack.
        def detach(token, fiber)
          OpenTelemetry::Context.detach(token) if Fiber.current.equal?(fiber)
        end
      end
    end
  end
end
