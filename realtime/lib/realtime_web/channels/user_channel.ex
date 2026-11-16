defmodule RealtimeWeb.UserChannel do
  @moduledoc """
  `user:<uid>`: events whose `topics` name this user (notifications,
  assignment). Only the user themself (`ws`, `kind=user`, `sub == uid`).
  """

  use Phoenix.Channel, log_join: false, log_handle_in: false

  # A scoped credential's socket receives only the events its scopes read
  # (`RealtimeWeb.Channels.EventScope`).
  intercept ["event"]

  alias RealtimeWeb.Channels.{EventScope, Guard}

  @impl true
  def join("user:" <> uid, _payload, %{assigns: %{kind: :user, sub: uid}} = socket) do
    if Guard.join_allowed?(socket),
      do: {:ok, Guard.ok_reply(), socket},
      else: {:error, %{reason: "rate_limited"}}
  end

  def join(_topic, _payload, _socket), do: Guard.forbidden(:user)

  @impl true
  def handle_out("event", payload, socket) do
    push(socket, "event", EventScope.visible(payload, socket))
    {:noreply, socket}
  end

  @impl true
  def handle_in(_event, _payload, socket) do
    case Guard.throttle_push(socket) do
      {:ok, socket} -> Guard.invalid(socket)
      {:drop, socket} -> {:noreply, socket}
      reply -> reply
    end
  end
end
