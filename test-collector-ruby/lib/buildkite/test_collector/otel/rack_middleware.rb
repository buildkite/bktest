# frozen_string_literal: true

module Buildkite
  module TestCollector
    module OTel
      # Wrap the instrumented app, outside OpenTelemetry's Rack middleware.
      # Only requests carrying a current local test's W3C trace context are
      # associated; headerless browser traffic must never guess an owner.
      class RackMiddleware
        def initialize(app)
          @app = app
        end

        def call(env)
          context = OTel.rack_context(env)
          return @app.call(env) unless context

          OpenTelemetry::Context.with_current(context) { @app.call(env) }
        end
      end
    end
  end
end
