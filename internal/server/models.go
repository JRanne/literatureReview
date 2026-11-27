package server

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"literaturereview/internal/ai"
	"literaturereview/internal/model"
)

// projectModels converts the project's saved choices into ai.Models.
func projectModels(p *model.Project) ai.Models {
	m := ai.Models{}
	if p == nil {
		return m
	}
	for _, s := range ai.AllSteps {
		if id := p.StepModel(string(s.Step)); id != "" {
			m[s.Step] = id
		}
	}
	return m
}

// client returns the AI client for one review step of a project: the
// project's pick, else MODEL_<STEP>, else the backend's base model.
func (s *Server) client(step ai.Step, p *model.Project) *ai.Client {
	return s.ai.ForStep(step, projectModels(p), s.cfg.StepModels)
}

// stepModelView is one row of the per-step model settings.
type stepModelView struct {
	ai.StepInfo
	Choice  string // the project's pick ("" = default)
	Default string // what "Default" resolves to right now
	Next    string // the model the next call of this step will use
}

func (s *Server) stepModelViews(p *model.Project) []stepModelView {
	var out []stepModelView
	for _, st := range ai.AllSteps {
		out = append(out, stepModelView{
			StepInfo: st,
			Choice:   p.StepModel(string(st.Step)),
			Default:  s.ai.ForStep(st.Step, nil, s.cfg.StepModels).ModelLabel(),
			Next:     s.client(st.Step, p).ModelLabel(),
		})
	}
	return out
}

// handleModels saves the per-step model choices of a project. Each step is
// posted as model_<step>; a step left out of the form keeps its choice.
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	picks := map[ai.Step]string{}
	for _, st := range ai.AllSteps {
		vals, ok := r.Form["model_"+string(st.Step)]
		if !ok {
			continue
		}
		v := strings.TrimSpace(vals[0])
		if !ai.KnownModel(v) {
			http.Redirect(w, r, "/projects/"+id+"?view=setup&error="+urlEsc(fmt.Sprintf("unknown model %q for %s", v, st.Label)), http.StatusSeeOther)
			return
		}
		picks[st.Step] = v
	}
	_ = s.store.SaveUndo(id, "Change models")
	err := s.store.Update(id, func(p *model.Project) error {
		if p.Job != nil && p.Job.Running {
			return errors.New("wait for the running job to finish before changing models")
		}
		for st, v := range picks {
			p.SetStepModel(string(st), v)
		}
		return nil
	})
	if err != nil {
		http.Redirect(w, r, "/projects/"+id+"?view=setup&error="+urlEsc(err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/projects/"+id+"?view=setup", http.StatusSeeOther)
}
