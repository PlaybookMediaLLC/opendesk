defmodule RealtimeWeb.WorkspaceChannel do
  @moduledoc """
  `workspace:<wid>`: every event of the workspace, both visibilities. Joined by
  `ws` tokens (users and API keys) that cover `wid`: the token's own workspace,
  or for a person one of the others its `workspaces` claim lists.
  """

  use Phoenix.Channel, log_join: false, log_handle_in: false

  # A scoped credential's socket receives only the events its scopes read
  # (`RealtimeWeb.Channels.EventScope`).
  intercept ["event"]

  alias Realtime.Auth
  alias RealtimeWeb.Channels.{EventScope, Guard}

  @impl true
  def join("workspace:" <> wid, _payload, %{assigns: %{kind: kind} = assigns} = socket)
      when kind in [:user, :api_key] do
    case Auth.workspace(assigns, wid) do
      nil ->
        Guard.forbidden(:workspace)

      _entry ->
        if Guard.join_allowed?(socket),
          do: {:ok, Guard.ok_reply(), socket},
          else: {:error, %{reason: "rate_limited"}}
    end
  end

  def join(_topic, _payload, _socket), do: Guard.forbidden(:workspace)

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
