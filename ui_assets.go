package main

import "fmt"

func formatScore(score float64) string {
	switch {
	case score >= 1000000:
		return fmt.Sprintf("%.2fm", score/1000000)
	case score >= 1000:
		return fmt.Sprintf("%.1fk", score/1000)
	default:
		return fmt.Sprintf("%.0f", score)
	}
}

const quotaGuardStatusScript = `<script>
(function(){
  const groupForm = document.getElementById("quota-guard-manual-group");
  const groupDetails = document.getElementById("quota-guard-manual-group-details");
  const message = document.getElementById("quota-guard-message");
  const refreshAll = document.getElementById("quota-guard-refresh-all");
  const bindingSelectAll = document.getElementById("quota-guard-bindings-all");
  const deleteBindings = document.getElementById("quota-guard-delete-bindings");
  const moveBindings = document.getElementById("quota-guard-move-bindings");
  const moveTarget = document.getElementById("quota-guard-move-target");
  const analyzeRebalance = document.getElementById("quota-guard-rebalance-analyze");
  const rebalanceOnce = document.getElementById("quota-guard-rebalance-once");
  async function action(data) {
    const params = new URLSearchParams();
    Object.keys(data || {}).forEach(function(key) {
      const value = data[key];
      if (Array.isArray(value)) {
        value.forEach(function(item) {
          if (item !== undefined && item !== "") params.append(key, String(item));
        });
      } else if (value !== undefined && value !== "") {
        params.set(key, String(value));
      }
    });
    const response = await fetch("/v0/resource/plugins/quota-guard/status?" + params.toString(), {method: "GET"});
    if (!response.ok) {
      let text = await response.text();
      try { text = JSON.parse(text).error || text; } catch (_) {}
      throw new Error(text || ("HTTP " + response.status));
    }
    return response;
  }
  async function refresh(data) {
    message.textContent = "Refreshing...";
    try {
      await action(Object.assign({action:"refresh"}, data || {}));
      message.textContent = "Refreshed.";
      setTimeout(function(){ location.reload(); }, 500);
    } catch (err) {
      message.textContent = err.message;
    }
  }
  if (refreshAll) refreshAll.addEventListener("click", function(){ refresh({all:true, force:true}); });
  if (analyzeRebalance) analyzeRebalance.addEventListener("click", async function() {
    message.textContent = "Analyzing Keeper usage...";
    try {
      await action({action:"rebalance-analyze"});
      message.textContent = "Rebalance analysis completed.";
      setTimeout(function(){ location.reload(); }, 400);
    } catch (err) {
      message.textContent = err.message;
    }
  });
  if (rebalanceOnce) rebalanceOnce.addEventListener("click", async function() {
    if (!confirm("Run one guarded rebalance move if an eligible idle client is found?")) return;
    message.textContent = "Running guarded rebalance...";
    try {
      await action({action:"rebalance-once"});
      message.textContent = "Rebalance cycle completed.";
      setTimeout(function(){ location.reload(); }, 400);
    } catch (err) {
      message.textContent = err.message;
    }
  });
  document.addEventListener("click", function(event) {
    const button = event.target.closest("button[data-refresh]");
    if (!button) return;
    refresh({auth_index: button.getAttribute("data-refresh"), force:true});
  });
  document.addEventListener("click", async function(event) {
    const button = event.target.closest("button[data-delete-auth]");
    if (!button) return;
    if (!confirm("Remove this local quota-guard state entry?")) return;
    message.textContent = "Removing...";
    try {
      await action({action:"delete-state", auth_id:button.getAttribute("data-delete-auth"), auth_index:button.getAttribute("data-delete-index")});
      message.textContent = "Removed.";
      setTimeout(function(){ location.reload(); }, 400);
    } catch (err) {
      message.textContent = err.message;
    }
  });
  document.addEventListener("click", async function(event) {
    const button = event.target.closest("button[data-delete-group]");
    if (!button) return;
    if (!confirm("Delete this manual group?")) return;
    message.textContent = "Deleting group...";
    try {
      await action({action:"delete-manual-group", group_id:button.getAttribute("data-delete-group")});
      message.textContent = "Deleted.";
      setTimeout(function(){ location.reload(); }, 400);
    } catch (err) {
      message.textContent = err.message;
    }
  });
  document.addEventListener("click", function(event) {
    const button = event.target.closest("button[data-create-group]");
    if (!button || !groupForm) return;
    const sourceGroup = button.getAttribute("data-create-group") || "group";
    const input = groupForm.querySelector("input[name=group_id]");
    if (input) input.value = "manual-" + sourceGroup.replace(/^auto-/, "");
    const members = (button.getAttribute("data-members") || "").split(",").filter(Boolean);
    groupForm.querySelectorAll("input[name=member]").forEach(function(box) {
      box.checked = members.indexOf(box.value) !== -1;
    });
    if (groupDetails) groupDetails.open = true;
    if (input) input.focus();
  });
  if (bindingSelectAll) bindingSelectAll.addEventListener("change", function() {
    document.querySelectorAll("input[name=quota-guard-client-binding]").forEach(function(box) {
      box.checked = bindingSelectAll.checked;
    });
  });
  if (deleteBindings) deleteBindings.addEventListener("click", async function() {
    const selected = Array.from(document.querySelectorAll("input[name=quota-guard-client-binding]:checked")).map(function(box) { return box.value; });
    if (selected.length === 0) {
      message.textContent = "Select client bindings to delete.";
      return;
    }
    if (!confirm("Delete selected client bindings?")) return;
    message.textContent = "Deleting client bindings...";
    try {
      await action({action:"delete-client-bindings", client_id:selected});
      message.textContent = "Client bindings deleted.";
      setTimeout(function(){ location.reload(); }, 400);
    } catch (err) {
      message.textContent = err.message;
    }
  });
  if (moveBindings) moveBindings.addEventListener("click", async function() {
    const selected = Array.from(document.querySelectorAll("input[name=quota-guard-client-binding]:checked")).map(function(box) { return box.value; });
    const groupID = moveTarget ? moveTarget.value : "";
    if (selected.length === 0) {
      message.textContent = "Select client bindings to move.";
      return;
    }
    if (!groupID) {
      message.textContent = "Select a target group.";
      return;
    }
    if (!confirm("Move selected client bindings to " + groupID + "?")) return;
    message.textContent = "Moving client bindings...";
    try {
      await action({action:"move-client-bindings", client_id:selected, group_id:groupID});
      message.textContent = "Client bindings moved.";
      setTimeout(function(){ location.reload(); }, 400);
    } catch (err) {
      message.textContent = err.message;
    }
  });
  if (groupForm) groupForm.addEventListener("submit", async function(event) {
    event.preventDefault();
    message.textContent = "Saving group...";
    const formData = new FormData(groupForm);
    const data = {group_id: formData.get("group_id"), member: formData.getAll("member")};
    try {
      await action(Object.assign({action:"save-manual-group"}, data));
      message.textContent = "Group saved.";
      setTimeout(function(){ location.reload(); }, 400);
    } catch (err) {
      message.textContent = err.message;
    }
  });
})();
</script>`
